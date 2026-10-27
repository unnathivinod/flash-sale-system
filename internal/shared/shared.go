// Package shared holds names and the Lua script used by both the API and the worker.
package shared

import (
	"os"
	"strconv"
)

// The one product in this flash sale.
const ProductID = 1

// Total phones available. Change with the TOTAL_STOCK env variable.
func TotalStock() int {
	if v, err := strconv.Atoi(os.Getenv("TOTAL_STOCK")); err == nil && v > 0 {
		return v
	}
	return 500
}

// Redis key names (one place, so API and worker always agree).
const (
	KeyStock     = "sale:stock"         // integer: tokens (phones) left
	KeyBuyers    = "sale:buyers"        // hash: userId -> orderId (one phone per user)
	KeyStatus    = "sale:order_status"  // hash: orderId -> QUEUED / CONFIRMED / FAILED
	KeyStream    = "sale:orders_stream" // Redis Stream = our message queue
	KeySaleStart = "sale:start_unix_ms" // when the sale opens (unix milliseconds)
	Group        = "order-workers"      // consumer group name for the workers
)

// ReserveLua runs INSIDE Redis as one atomic step.
// Redis executes one command/script at a time, so no two requests can
// interleave here. That is what makes overselling impossible.
//
// KEYS[1]=stock  KEYS[2]=buyers  KEYS[3]=status  KEYS[4]=stream
// ARGV[1]=userId ARGV[2]=new orderId ARGV[3]=productId
//
// Returns {code, orderId}:
//
//	1 = reserved now      2 = this user already reserved (same orderId returned)
//	0 = sold out
const ReserveLua = `
local existing = redis.call('HGET', KEYS[2], ARGV[1])
if existing then
  return {2, existing}                      -- double click / retry: same order, no new stock taken
end

local stock = tonumber(redis.call('GET', KEYS[1]) or '0')
if stock <= 0 then
  return {0, ''}                            -- sold out
end

redis.call('DECR', KEYS[1])                 -- take one token
redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[2], 'QUEUED')
redis.call('XADD', KEYS[4], '*',            -- put the order on the queue
  'order_id', ARGV[2], 'user_id', ARGV[1], 'product_id', ARGV[3])
return {1, ARGV[2]}
`

// Env returns an environment variable or a default.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
