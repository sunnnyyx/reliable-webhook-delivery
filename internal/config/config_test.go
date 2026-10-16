package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://user:secret@localhost:5432/relayforge?sslmode=disable"}
	tests := []struct{ name, key, value, want string }{
		{"defaults", "", "", ""}, {"missing db", "DATABASE_URL", "", "DATABASE_URL"}, {"bad scheme", "DATABASE_URL", "https://localhost/db", "DATABASE_URL"},
		{"bad profile", "PROFILE", "staging", "PROFILE"}, {"bad port", "API_ADDR", ":0", "listen"}, {"bad worker", "WORKER_CONCURRENCY", "0", "WORKER_CONCURRENCY"},
		{"huge pool", "DB_MAX_CONNS", "101", "DB_MAX_CONNS"}, {"negative timeout", "READY_TIMEOUT", "-1s", "READY_TIMEOUT"}, {"huge timeout", "SHUTDOWN_TIMEOUT", "2m", "SHUTDOWN_TIMEOUT"},
		{"bad poll", "QUEUE_POLL_INTERVAL", "oops", "QUEUE_POLL_INTERVAL"}, {"insecure production", "PROFILE", "production", "verify-full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range base {
				m[k] = v
			}
			if tt.key != "" {
				m[tt.key] = tt.value
			}
			c, e := Load(func(k string) string { return m[k] })
			if tt.want == "" {
				if e != nil {
					t.Fatal(e)
				}
				if c.WorkerConcurrency != 8 || c.DBMaxConns != 10 {
					t.Fatal(c)
				}
			} else if e == nil || !strings.Contains(e.Error(), tt.want) {
				t.Fatalf("error=%v", e)
			}
			if e != nil && strings.Contains(e.Error(), "secret") {
				t.Fatal("credential leaked")
			}
		})
	}
}
func TestProduction(t *testing.T) {
	_, e := Load(func(k string) string {
		switch k {
		case "PROFILE":
			return "production"
		case "DATABASE_URL":
			return "postgres://user:pass@db/relayforge?sslmode=verify-full"
		}
		return ""
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestRateLimitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		want       bool
	}{
		{"RATE_LIMIT_ENABLED", "maybe", false}, {"REDIS_URL", "", false}, {"REDIS_URL", "https://user:private@redis", false},
		{"TENANT_RATE_PER_SECOND", "0", false}, {"TENANT_BURST", "10001", false}, {"RATE_LIMIT_PREFIX", "unsafe:prefix", false},
		{"MAX_TENANT_IN_FLIGHT", "0", false}, {"MAX_TENANT_BACKLOG", "-1", false}, {"REDIS_URL", "redis://localhost:16379/0", true},
	} {
		m := map[string]string{"DATABASE_URL": "postgres://user:private@db/test", "RATE_LIMIT_ENABLED": "true", "REDIS_URL": "redis://localhost:16379/0"}
		m[tc.key] = tc.value
		c, e := Load(func(k string) string { return m[k] })
		if (e == nil) != tc.want {
			t.Fatalf("%s: %v", tc.key, e)
		}
		if e != nil && strings.Contains(e.Error(), "private") {
			t.Fatal("configuration leaked credentials")
		}
		if e == nil && (!c.RateLimitEnabled || c.TenantRate != 10 || c.TenantBurst != 20 || c.MaxTenantInFlight != 4 || c.MaxTenantBacklog != 10000) {
			t.Fatal("bad defaults")
		}
	}
}

func TestTelemetryConfigurationAndPrivateListeners(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		valid      bool
	}{
		{"API_OPS_ADDR", "0.0.0.0:9091", false}, {"WORKER_ADDR", ":8081", false}, {"API_OPS_ADDR", "localhost:9091", false}, {"API_OPS_ADDR", "[::1]:9091", true},
		{"TRACE_SAMPLE_RATIO", "NaN", false}, {"TRACE_SAMPLE_RATIO", "2", false}, {"TRACE_SAMPLE_RATIO", "0", true},
		{"TRACE_ENDPOINT", "http://user:private@collector/v1/traces", false}, {"TRACE_ENDPOINT", "http://collector/v1/traces?private=1", false}, {"TRACE_ENDPOINT", "http://127.0.0.1:4318/v1/traces", true},
	} {
		m := map[string]string{"DATABASE_URL": "postgres://user:private@localhost/test"}
		m[tc.key] = tc.value
		_, e := Load(func(k string) string { return m[k] })
		if (e == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.key, e)
		}
		if e != nil && strings.Contains(e.Error(), "private@") {
			t.Fatal("credential disclosure")
		}
	}
}
