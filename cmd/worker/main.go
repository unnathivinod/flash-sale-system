// Worker: reads orders from the Redis Stream queue and writes them to
// Postgres, OUTSIDE the HTTP request. Users never wait on the database.
//
// Safety layers in the database (even if Redis were wrong):
//  1. UPDATE ... WHERE stock > 0   -> can never go below zero
//  2. CHECK (stock >= 0)           -> database refuses negative stock
//  3. UNIQUE (user_id, product_id) -> one phone per user
//  4. order_id PRIMARY KEY         -> a redelivered message is not processed twice
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"flashsale/internal/shared"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	rdb := redis.NewClient(&redis.Options{
		Addr:     shared.Env("REDIS_ADDR", "localhost:6379"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	db, err := pgxpool.New(ctx, shared.Env("DATABASE_URL", "postgres://flash:flash@localhost:5432/flashsale"))
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; ; i++ {
		if rdb.Ping(ctx).Err() == nil && db.Ping(ctx) == nil {
			break
		}
		if i > 60 {
			log.Fatal("redis/postgres not reachable")
		}
		time.Sleep(time.Second)
	}
	ensureGroup(ctx, rdb)

	n, _ := strconv.Atoi(shared.Env("WORKER_CONCURRENCY", "4"))
	host, _ := os.Hostname()
	var wg sync.WaitGroup
	for i := 0; i < max(1, n); i++ {
		w := &worker{rdb: rdb, db: db, name: fmt.Sprintf("%s-%d", host, i)}
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}
	log.Printf("worker %s started with %d consumers", host, n)
	wg.Wait()
}

type worker struct {
	rdb  *redis.Client
	db   *pgxpool.Pool
	name string // unique consumer name inside the group
}

func (w *worker) run(ctx context.Context) {
	lastClaim := time.Time{}
	for ctx.Err() == nil {
		// Every 10s, take over messages a crashed worker read but never finished.
		if time.Since(lastClaim) > 10*time.Second {
			w.reclaim(ctx)
			lastClaim = time.Now()
		}

		streams, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: shared.Group, Consumer: w.name,
			Streams: []string{shared.KeyStream, ">"}, // ">" = messages nobody has taken yet
			Count:   50, Block: 2 * time.Second,
		}).Result()
		if errors.Is(err, redis.Nil) || ctx.Err() != nil {
			continue // nothing new
		}
		if err != nil {
			if strings.Contains(err.Error(), "NOGROUP") { // queue was reset by the demo
				ensureGroup(ctx, w.rdb)
			} else {
				log.Println("read:", err)
				time.Sleep(time.Second)
			}
			continue
		}
		for _, s := range streams {
			for _, m := range s.Messages {
				w.handle(ctx, m)
			}
		}
	}
}

func (w *worker) reclaim(ctx context.Context) {
	msgs, _, err := w.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: shared.KeyStream, Group: shared.Group, Consumer: w.name,
		MinIdle: 30 * time.Second, Start: "0", Count: 100,
	}).Result()
	if err != nil {
		return
	}
	for _, m := range msgs {
		w.handle(ctx, m)
	}
}

// handle processes ONE order inside ONE database transaction.
func (w *worker) handle(ctx context.Context, m redis.XMessage) {
	orderID, _ := m.Values["order_id"].(string)
	userID, _ := m.Values["user_id"].(string)
	productID, _ := strconv.Atoi(fmt.Sprint(m.Values["product_id"]))

	status, err := w.persist(ctx, orderID, userID, productID)
	if err != nil {
		// Database hiccup: do NOT acknowledge. The message stays pending and
		// is retried by reclaim(). Nothing is lost.
		log.Printf("order %s: %v (will retry)", orderID, err)
		time.Sleep(200 * time.Millisecond)
		return
	}

	pipe := w.rdb.TxPipeline()
	pipe.HSet(ctx, shared.KeyStatus, orderID, status)
	if status == "REJECTED_DUPLICATE" {
		pipe.Incr(ctx, shared.KeyStock) // give the token back
	}
	pipe.XAck(ctx, shared.KeyStream, shared.Group, m.ID) // done, remove from pending
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("order %s: ack failed: %v", orderID, err)
	}
}

func (w *worker) persist(ctx context.Context, orderID, userID string, productID int) (string, error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	// Already processed? (a message can be delivered twice after a crash)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE order_id=$1)`, orderID).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "CONFIRMED", nil
	}

	// THE key statement from the case study: decrement only if stock > 0.
	// Postgres runs this check-and-update as one atomic step.
	tag, err := tx.Exec(ctx,
		`UPDATE inventory SET stock = stock - 1 WHERE product_id = $1 AND stock > 0`, productID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "FAILED_SOLD_OUT", tx.Commit(ctx)
	}

	tag, err = tx.Exec(ctx,
		`INSERT INTO orders(order_id, user_id, product_id) VALUES($1,$2,$3)
		 ON CONFLICT (user_id, product_id) DO NOTHING`, orderID, userID, productID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		// This user already owns one. Undo the decrement by rolling back.
		tx.Rollback(ctx)
		return "REJECTED_DUPLICATE", nil
	}
	return "CONFIRMED", tx.Commit(ctx)
}

func ensureGroup(ctx context.Context, rdb *redis.Client) {
	err := rdb.XGroupCreateMkStream(ctx, shared.KeyStream, shared.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		log.Println("create group:", err)
	}
}
