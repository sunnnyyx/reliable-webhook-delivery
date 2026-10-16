package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/url"
	"relayforge/internal/netpolicy"
	"relayforge/internal/ratelimit"
	"strconv"
	"time"
)

type Config struct {
	APIOpsAddr, TraceEndpoint string
	TraceSampleRatio          float64

	RateLimitEnabled                                             bool
	RedisURL, RateLimitPrefix                                    string
	TenantRate, TenantBurst, MaxTenantInFlight, MaxTenantBacklog int

	Profile, APIAddr, WorkerAddr, DatabaseURL                   string
	DBMaxConns                                                  int32
	StartupTimeout, ReadyTimeout, ShutdownTimeout, PollInterval time.Duration
	WorkerConcurrency                                           int
	EncryptionKey                                               []byte
	DevReceiverURL                                              string
	RequestTimeout, LeaseDuration                               time.Duration
}

// Load accepts a lookup function so tests do not mutate process environment.
func Load(get func(string) string) (Config, error) {
	value := func(k, fallback string) string {
		if v := get(k); v != "" {
			return v
		}
		return fallback
	}
	c := Config{Profile: value("PROFILE", "development"), APIAddr: value("API_ADDR", ":8080"), WorkerAddr: value("WORKER_ADDR", "127.0.0.1:8081"), DatabaseURL: get("DATABASE_URL")}
	if c.Profile != "development" && c.Profile != "production" {
		return c, fmt.Errorf("PROFILE must be development or production")
	}
	for _, a := range []string{c.APIAddr, c.WorkerAddr} {
		_, port, e := net.SplitHostPort(a)
		n, ne := strconv.Atoi(port)
		if e != nil || ne != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("listen addresses require host:port with port 1–65535")
		}
	}
	c.APIOpsAddr = value("API_OPS_ADDR", "127.0.0.1:9091")
	for _, address := range []string{c.APIOpsAddr, c.WorkerAddr} {
		host, port, e := net.SplitHostPort(address)
		n, ne := strconv.Atoi(port)
		ip := net.ParseIP(host)
		if e != nil || ne != nil || n < 1 || n > 65535 || ip == nil || !ip.IsLoopback() {
			return c, fmt.Errorf("operational addresses must use literal loopback and a valid port")
		}
	}
	c.TraceEndpoint = get("TRACE_ENDPOINT")
	ratio, ratioErr := strconv.ParseFloat(value("TRACE_SAMPLE_RATIO", "0.1"), 64)
	c.TraceSampleRatio = ratio
	if ratioErr != nil || math.IsNaN(c.TraceSampleRatio) || math.IsInf(c.TraceSampleRatio, 0) || c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return c, fmt.Errorf("TRACE_SAMPLE_RATIO must be in [0,1]")
	}
	if c.TraceEndpoint != "" {
		endpoint, e := url.Parse(c.TraceEndpoint)
		if e != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return c, fmt.Errorf("TRACE_ENDPOINT must be an explicit HTTP(S) traces URL without credentials/query/fragment")
		}
		if c.Profile == "production" && endpoint.Scheme != "https" {
			return c, fmt.Errorf("production TRACE_ENDPOINT requires HTTPS")
		}
	}
	u, e := url.Parse(c.DatabaseURL)
	if e != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
		return c, fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with host and database")
	}
	if c.Profile == "production" && u.Query().Get("sslmode") != "verify-full" {
		return c, fmt.Errorf("production DATABASE_URL requires sslmode=verify-full")
	}
	integer := func(k string, fallback, min, max int) (int, error) {
		n, e := strconv.Atoi(value(k, strconv.Itoa(fallback)))
		if e != nil || n < min || n > max {
			return 0, fmt.Errorf("%s must be an integer in [%d,%d]", k, min, max)
		}
		return n, nil
	}
	n, e := integer("DB_MAX_CONNS", 10, 1, 100)
	if e != nil {
		return c, e
	}
	c.DBMaxConns = int32(n)
	c.WorkerConcurrency, e = integer("WORKER_CONCURRENCY", 8, 1, 128)
	if e != nil {
		return c, e
	}
	for _, field := range []struct {
		k, def   string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"STARTUP_TIMEOUT", "5s", &c.StartupTimeout, time.Millisecond, 30 * time.Second},
		{"READY_TIMEOUT", "1s", &c.ReadyTimeout, time.Millisecond, 5 * time.Second},
		{"SHUTDOWN_TIMEOUT", "15s", &c.ShutdownTimeout, time.Millisecond, time.Minute},
		{"QUEUE_POLL_INTERVAL", "500ms", &c.PollInterval, 10 * time.Millisecond, time.Minute},
	} {
		d, e := time.ParseDuration(value(field.k, field.def))
		if e != nil || d < field.min || d > field.max {
			return c, fmt.Errorf("%s must be a duration in [%s,%s]", field.k, field.min, field.max)
		}
		*field.dst = d
	}
	c.RequestTimeout = 10 * time.Second
	c.LeaseDuration = 30 * time.Second
	for _, field := range []struct {
		key      string
		dst      *time.Duration
		min, max time.Duration
	}{{"OUTBOUND_TIMEOUT", &c.RequestTimeout, 100 * time.Millisecond, 30 * time.Second}, {"DELIVERY_LEASE", &c.LeaseDuration, time.Second, 2 * time.Minute}} {
		if raw := get(field.key); raw != "" {
			d, e := time.ParseDuration(raw)
			if e != nil || d < field.min || d > field.max {
				return c, fmt.Errorf("invalid %s duration", field.key)
			}
			*field.dst = d
		}
	}
	if c.LeaseDuration <= c.RequestTimeout+6*time.Second {
		return c, fmt.Errorf("DELIVERY_LEASE must exceed OUTBOUND_TIMEOUT plus 6s for persistence margin")
	}
	c.DevReceiverURL = get("DEV_RECEIVER_URL")
	if _, e := netpolicy.New(c.Profile, c.DevReceiverURL, nil, nil); e != nil {
		return c, e
	}
	if raw := value("RATE_LIMIT_ENABLED", "false"); raw != "true" && raw != "false" {
		return c, fmt.Errorf("RATE_LIMIT_ENABLED must be true or false")
	} else {
		c.RateLimitEnabled = raw == "true"
	}
	c.RedisURL = get("REDIS_URL")
	c.RateLimitPrefix = value("RATE_LIMIT_PREFIX", "relayforge-v1")
	if !ratelimit.ValidPrefix(c.RateLimitPrefix) {
		return c, fmt.Errorf("invalid RATE_LIMIT_PREFIX")
	}
	for _, field := range []struct {
		key                string
		dst                *int
		fallback, min, max int
	}{
		{"TENANT_RATE_PER_SECOND", &c.TenantRate, 10, 1, 10000}, {"TENANT_BURST", &c.TenantBurst, 20, 1, 10000},
		{"MAX_TENANT_IN_FLIGHT", &c.MaxTenantInFlight, 4, 1, 128}, {"MAX_TENANT_BACKLOG", &c.MaxTenantBacklog, 10000, 1, 1000000},
	} {
		*field.dst, e = integer(field.key, field.fallback, field.min, field.max)
		if e != nil {
			return c, e
		}
	}
	if c.RateLimitEnabled {
		u, e := url.Parse(c.RedisURL)
		if e != nil || u == nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
			return c, fmt.Errorf("enabled rate limiting requires valid REDIS_URL")
		}
		if c.Profile == "production" && u.Scheme != "rediss" {
			return c, fmt.Errorf("production Redis requires rediss TLS")
		}
		client, e := ratelimit.New(c.RedisURL, c.RateLimitPrefix, c.TenantRate, c.TenantBurst)
		if e != nil {
			return c, fmt.Errorf("invalid Redis configuration")
		}
		client.Close()
	}
	if raw := get("ENCRYPTION_KEY"); raw != "" {
		key, e := base64.StdEncoding.Strict().DecodeString(raw)
		if e != nil || len(key) != 32 {
			return c, fmt.Errorf("ENCRYPTION_KEY must be base64 encoding of 32 bytes")
		}
		c.EncryptionKey = key
	}
	return c, nil
}
