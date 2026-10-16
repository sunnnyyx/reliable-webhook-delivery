package ratelimit

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"relayforge/internal/security"
	"relayforge/internal/telemetry"
	"strings"
	"sync/atomic"
	"time"
)

type Decision struct {
	Allowed bool
	Wait    time.Duration
}
type Limiter interface {
	Allow(context.Context, string) (Decision, error)
}

// Redis stores only ephemeral permissions, never delivery work. All instances
// for one sender deployment must share namespace, rate and burst settings.
type Redis struct {
	Telemetry *telemetry.Runtime

	Client                  *redis.Client
	Prefix                  string
	Rate, Burst             int
	Allowed, Denied, Errors atomic.Uint64
}

func New(url, prefix string, rate, burst int) (*Redis, error) {
	options, e := redis.ParseURL(url)
	if e != nil || !ValidPrefix(prefix) || rate < 1 || rate > 10000 || burst < 1 || burst > 10000 {
		return nil, errors.New("invalid Redis limiter configuration")
	}
	options.MaxRetries = -1 // A lost reply may have consumed a token; do not replay admission.
	options.DialTimeout = 100 * time.Millisecond
	options.ReadTimeout = 100 * time.Millisecond
	options.WriteTimeout = 100 * time.Millisecond
	options.PoolTimeout = 100 * time.Millisecond
	options.ContextTimeoutEnabled = true
	options.PoolSize = 16
	return &Redis{Client: redis.NewClient(options), Prefix: prefix, Rate: rate, Burst: burst}, nil
}
func ValidPrefix(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-", c)) {
			return false
		}
	}
	return true
}

// Redis TIME avoids disagreeing worker clocks. Keep a future last_refill on a
// backward clock step (conservative delay), cap capacity, and expire only after
// enough idle time to refill completely. Changed settings on an existing bucket
// fail closed rather than silently giving a second burst.
var bucket = redis.NewScript(`
local tm=redis.call('TIME')
local now=tonumber(tm[1])*1000+math.floor(tonumber(tm[2])/1000)
local rate=tonumber(ARGV[1])
local capacity=tonumber(ARGV[2])
local state=redis.call('HMGET',KEYS[1],'tokens','last','rate','capacity')
local tokens=tonumber(state[1])
local last=tonumber(state[2])
if state[1] or state[2] or state[3] or state[4] then
 if tokens==nil or last==nil or tokens~=tokens or last~=last or tokens<0 or tokens>capacity or not state[3] or not state[4] then
  return redis.error_reply('INVALID_BUCKET_STATE')
 end
else tokens=capacity; last=now end
if state[3] and (tonumber(state[3])~=rate or tonumber(state[4])~=capacity) then
 return redis.error_reply('BUCKET_CONFIGURATION_MISMATCH')
end
local elapsed=math.max(0,now-last)
tokens=math.min(capacity,tokens+elapsed*rate/1000)
last=math.max(last,now)
local allowed=0
local wait=0
if tokens>=1 then tokens=tokens-1;allowed=1
else wait=math.ceil((1-tokens)*1000/rate)+math.max(0,last-now) end
redis.call('HSET',KEYS[1],'tokens',tokens,'last',last,'rate',rate,'capacity',capacity)
redis.call('PEXPIRE',KEYS[1],math.max(1000,math.ceil(capacity*1000/rate)*2+math.max(0,last-now)))
return {allowed,wait}
`)

func (r *Redis) Allow(ctx context.Context, tenant string) (Decision, error) {
	if !security.ValidID(tenant) {
		return Decision{}, errors.New("invalid limiter tenant")
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	values, e := bucket.Run(bounded, r.Client, []string{r.Prefix + ":tenant:" + tenant}, r.Rate, r.Burst).Int64Slice()
	if e != nil || len(values) != 2 || values[0] < 0 || values[0] > 1 || values[1] < 0 || values[1] > 3600000 {
		r.Errors.Add(1)
		r.Telemetry.Count(ctx, "rate_limit_errors", "", "")
		return Decision{}, errors.New("limiter unavailable")
	}
	decision := Decision{Allowed: values[0] == 1, Wait: time.Duration(values[1]) * time.Millisecond}
	if decision.Allowed {
		r.Allowed.Add(1)
		r.Telemetry.Count(ctx, "rate_limit_permissions", "", "")
	} else {
		r.Denied.Add(1)
		r.Telemetry.Count(ctx, "rate_limit_denials", "", "")
	}
	return decision, nil
}
func (r *Redis) Ping(ctx context.Context) error { return r.Client.Ping(ctx).Err() }
func (r *Redis) Close() error                   { return r.Client.Close() }
