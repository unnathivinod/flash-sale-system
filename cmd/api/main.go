// API server. It is STATELESS: everything shared lives in Redis/Postgres,
// so we can run as many copies as we like behind a load balancer.
//
// The checkout path never touches the database. It runs one atomic Redis
// script, pushes the order onto a queue, and replies "202 Accepted" in ~1ms.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"flashsale/internal/shared"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type server struct {
	rdb        *redis.Client
	db         *pgxpool.Pool
	reserve    *redis.Script
	adminToken string

	// Optional per-IP rate limit inside the app (used on AWS, where there
	// is no NGINX). Counters live in Redis so all replicas share them.
	rateLimit   int
	loadTestKey string
}

func main() {
	ctx := context.Background()

	rdb := redis.NewClient(&redis.Options{
		Addr:     shared.Env("REDIS_ADDR", "localhost:6379"),
		Password: os.Getenv("REDIS_PASSWORD"),
		PoolSize: 200, // many concurrent requests share these connections
	})
	db, err := pgxpool.New(ctx, shared.Env("DATABASE_URL", "postgres://flash:flash@localhost:5432/flashsale"))
	if err != nil {
		log.Fatal(err)
	}
	waitFor(ctx, rdb, db)

	rl, _ := strconv.Atoi(shared.Env("RATE_LIMIT_PER_SEC", "0"))
	s := &server{
		rdb:         rdb,
		db:          db,
		reserve:     redis.NewScript(shared.ReserveLua),
		adminToken:  shared.Env("ADMIN_TOKEN", "change-me"),
		rateLimit:   rl,
		loadTestKey: shared.Env("LOADTEST_KEY", "local-loadtest-only"),
	}
	s.bootstrap(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/sale", s.sale)
	mux.HandleFunc("POST /api/checkout", s.checkout)
	mux.HandleFunc("GET /api/orders/{id}", s.orderStatus)
	mux.HandleFunc("POST /api/naive/checkout", s.naiveCheckout) // the BROKEN design, for comparison
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("POST /api/admin/reset", s.reset)

	// On AWS the API also serves the frontend (no NGINX there).
	if dir := os.Getenv("STATIC_DIR"); dir != "" {
		mux.Handle("/", staticHandler(dir))
	}

	srv := &http.Server{
		Addr:              ":" + shared.Env("PORT", "8080"),
		Handler:           s.rateLimiter(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown: finish in-flight requests when the container stops.
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()

	host, _ := os.Hostname()
	log.Printf("api %s listening on %s", host, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// bootstrap prepares Redis the first time. Postgres is the source of truth:
// if Redis has no stock key (fresh start or Redis lost data), we copy the
// remaining stock from the database. SETNX means "only if it doesn't exist",
// so several replicas starting together cannot overwrite each other.
func (s *server) bootstrap(ctx context.Context) {
	var dbStock int
	if err := s.db.QueryRow(ctx, `SELECT stock FROM inventory WHERE product_id=$1`, shared.ProductID).Scan(&dbStock); err != nil {
		log.Fatal("read inventory: ", err)
	}
	s.rdb.SetNX(ctx, shared.KeyStock, dbStock, 0)
	start := time.Now().Add(saleDelay()).UnixMilli()
	s.rdb.SetNX(ctx, shared.KeySaleStart, start, 0)
	createGroup(ctx, s.rdb)
}

func createGroup(ctx context.Context, rdb *redis.Client) {
	err := rdb.XGroupCreateMkStream(ctx, shared.KeyStream, shared.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		log.Println("create group:", err)
	}
}

func saleDelay() time.Duration {
	sec, err := strconv.Atoi(shared.Env("SALE_DELAY_SEC", "30"))
	if err != nil {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// ---------------------------------------------------------------- handlers

// GET /api/sale: what the page polls. Cheap: two Redis reads.
func (s *server) sale(w http.ResponseWriter, r *http.Request) {
	vals, err := s.rdb.MGet(r.Context(), shared.KeySaleStart, shared.KeyStock).Result()
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": "unavailable"})
		return
	}
	start := toInt(vals[0])
	stock := toInt(vals[1])
	now := time.Now().UnixMilli()
	status := "UPCOMING"
	if now >= start {
		status = "LIVE"
		if stock <= 0 {
			status = "SOLD_OUT"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"productId": shared.ProductID, "startsAt": start, "serverTime": now,
		"stockLeft": stock, "totalStock": shared.TotalStock(), "status": status,
	})
}

type checkoutReq struct {
	UserID string `json:"userId"`
}

// POST /api/checkout: the hot path. No database here.
func (s *server) checkout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req checkoutReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || !validUser(req.UserID) {
		writeJSON(w, 400, map[string]any{"error": "invalid userId"})
		return
	}

	// Sale not open yet?
	start, err := s.rdb.Get(ctx, shared.KeySaleStart).Int64()
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": "unavailable"})
		return
	}
	if time.Now().UnixMilli() < start {
		writeJSON(w, 425, map[string]any{"status": "NOT_STARTED", "startsAt": start})
		return
	}

	// THE atomic step: check stock + take token + enqueue, all in one go.
	res, err := s.reserve.Run(ctx, s.rdb,
		[]string{shared.KeyStock, shared.KeyBuyers, shared.KeyStatus, shared.KeyStream},
		req.UserID, newOrderID(), shared.ProductID).Slice()
	if err != nil {
		log.Println("reserve:", err)
		writeJSON(w, 503, map[string]any{"error": "try again"})
		return
	}

	code := toInt(res[0])
	orderID, _ := res[1].(string)
	switch code {
	case 1, 2: // reserved now (1) or already reserved earlier (2, double click)
		w.Header().Set("Location", "/api/orders/"+orderID)
		writeJSON(w, 202, map[string]any{
			"status": "QUEUED", "orderId": orderID, "trackUrl": "/api/orders/" + orderID,
			"duplicate": code == 2,
		})
	default: // sold out
		writeJSON(w, 409, map[string]any{"status": "SOLD_OUT"})
	}
}

// GET /api/orders/{id}: the page polls this after a 202.
func (s *server) orderStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, err := s.rdb.HGet(r.Context(), shared.KeyStatus, id).Result()
	if errors.Is(err, redis.Nil) {
		writeJSON(w, 404, map[string]any{"error": "unknown order"})
		return
	} else if err != nil {
		writeJSON(w, 503, map[string]any{"error": "unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"orderId": id, "status": st})
}

// POST /api/naive/checkout: the ORIGINAL buggy design, kept on purpose so we
// can prove the bug. It does "SELECT stock, then UPDATE" with no lock and no
// condition. Two requests can read stock=1 at the same time, both "succeed",
// and we sell more phones than we have (the 540-vs-500 bug).
func (s *server) naiveCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req checkoutReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || !validUser(req.UserID) {
		writeJSON(w, 400, map[string]any{"error": "invalid userId"})
		return
	}

	var stock int
	if err := s.db.QueryRow(ctx, `SELECT stock FROM naive_inventory WHERE product_id=$1`, shared.ProductID).Scan(&stock); err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	if stock <= 0 {
		writeJSON(w, 409, map[string]any{"status": "SOLD_OUT"})
		return
	}

	time.Sleep(2 * time.Millisecond) // pretend to validate payment etc.

	// BUG: writes back a value computed from a stale read ("lost update").
	if _, err := s.db.Exec(ctx, `UPDATE naive_inventory SET stock=$1 WHERE product_id=$2`, stock-1, shared.ProductID); err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO naive_orders(user_id) VALUES($1)`, req.UserID); err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "CONFIRMED"})
}

// GET /api/stats: numbers for the live dashboard and for verifying tests.
func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{"totalStock": shared.TotalStock()}

	out["redisStockLeft"], _ = s.rdb.Get(ctx, shared.KeyStock).Int()
	out["reserved"], _ = s.rdb.HLen(ctx, shared.KeyBuyers).Result()
	if p, err := s.rdb.XPending(ctx, shared.KeyStream, shared.Group).Result(); err == nil {
		out["queuePending"] = p.Count
	}
	out["queueLength"], _ = s.rdb.XLen(ctx, shared.KeyStream).Result()

	var dbStock, confirmed, naiveStock, naiveOrders int
	s.db.QueryRow(ctx, `SELECT stock FROM inventory WHERE product_id=$1`, shared.ProductID).Scan(&dbStock)
	s.db.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&confirmed)
	s.db.QueryRow(ctx, `SELECT stock FROM naive_inventory WHERE product_id=$1`, shared.ProductID).Scan(&naiveStock)
	s.db.QueryRow(ctx, `SELECT count(*) FROM naive_orders`).Scan(&naiveOrders)

	out["dbStockLeft"] = dbStock
	out["confirmedOrders"] = confirmed
	out["oversold"] = confirmed > shared.TotalStock()
	out["naive"] = map[string]any{
		"dbStockLeft": naiveStock, "orders": naiveOrders,
		"oversold": naiveOrders > shared.TotalStock(), "oversoldBy": max(0, naiveOrders-shared.TotalStock()),
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

// POST /api/admin/reset?delay=10: restart the sale for another demo run.
func (s *server) reset(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Admin-Token") != s.adminToken {
		writeJSON(w, 403, map[string]any{"error": "forbidden"})
		return
	}
	ctx := r.Context()
	total := shared.TotalStock()
	delay := saleDelay()
	if d, err := strconv.Atoi(r.URL.Query().Get("delay")); err == nil && d >= 0 {
		delay = time.Duration(d) * time.Second
	}

	tx, err := s.db.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, `TRUNCATE orders, naive_orders`)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE inventory SET stock=$1 WHERE product_id=$2`, total, shared.ProductID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE naive_inventory SET stock=$1 WHERE product_id=$2`, total, shared.ProductID)
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}

	start := time.Now().Add(delay).UnixMilli()
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, shared.KeyBuyers, shared.KeyStatus, shared.KeyStream)
	pipe.Set(ctx, shared.KeyStock, total, 0)
	pipe.Set(ctx, shared.KeySaleStart, start, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	createGroup(ctx, s.rdb)
	writeJSON(w, 200, map[string]any{"ok": true, "stock": total, "startsAt": start})
}

// ---------------------------------------------------------------- middleware

// rateLimiter: fixed 1-second window per client IP, counted in Redis so all
// replicas share the same counter. Disabled when RATE_LIMIT_PER_SEC=0
// (locally NGINX does this job instead).
func (s *server) rateLimiter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimit <= 0 || !strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("X-Loadtest-Key") == s.loadTestKey {
			next.ServeHTTP(w, r)
			return
		}
		if isBot(r.UserAgent()) {
			writeJSON(w, 403, map[string]any{"error": "blocked"})
			return
		}
		key := "rl:" + clientIP(r) + ":" + strconv.FormatInt(time.Now().Unix(), 10)
		pipe := s.rdb.Pipeline()
		incr := pipe.Incr(r.Context(), key)
		pipe.Expire(r.Context(), key, 2*time.Second)
		if _, err := pipe.Exec(r.Context()); err == nil && incr.Val() > int64(s.rateLimit) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, map[string]any{"error": "too many requests"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isBot(ua string) bool {
	ua = strings.ToLower(ua)
	if ua == "" {
		return true
	}
	for _, b := range []string{"python-requests", "scrapy", "aiohttp", "httpclient", "bot", "spider", "crawler"} {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	// The load balancer APPENDS the real client IP as the LAST entry.
	// Earlier entries could be faked by the client, so we ignore them.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func staticHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		fs.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- helpers

func waitFor(ctx context.Context, rdb *redis.Client, db *pgxpool.Pool) {
	for i := 0; i < 60; i++ {
		if rdb.Ping(ctx).Err() == nil && db.Ping(ctx) == nil {
			return
		}
		log.Println("waiting for redis/postgres...")
		time.Sleep(time.Second)
	}
	log.Fatal("redis/postgres not reachable")
}

func validUser(u string) bool {
	if len(u) == 0 || len(u) > 64 {
		return false
	}
	for _, c := range u {
		if !(c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func newOrderID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "ord_" + hex.EncodeToString(b)
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
