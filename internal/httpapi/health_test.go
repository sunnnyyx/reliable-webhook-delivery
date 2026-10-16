package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(c context.Context) error { return f(c) }
func TestHealth(t *testing.T) {
	for _, tc := range []struct {
		path string
		fail bool
		want int
	}{{"/health/live", true, 200}, {"/health/ready", false, 200}, {"/health/ready", true, 503}} {
		calls := 0
		h := Health(pingFunc(func(context.Context) error {
			calls++
			if tc.fail {
				return errors.New("password=hidden")
			}
			return nil
		}), time.Second)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.want || len(w.Header().Get("X-Request-ID")) != 32 {
			t.Fatal(w)
		}
		if strings.Contains(w.Body.String(), "hidden") {
			t.Fatal("leak")
		}
		if tc.path == "/health/live" && calls != 0 {
			t.Fatal("liveness probed DB")
		}
	}
}
func TestReadyDeadline(t *testing.T) {
	h := Health(pingFunc(func(c context.Context) error { <-c.Done(); return c.Err() }), 10*time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestLimiterDegradesWorkerReadinessOnly(t *testing.T) {
	calls := 0
	h := Health(pingFunc(func(context.Context) error { return nil }), time.Second, pingFunc(func(context.Context) error { calls++; return errors.New("private Redis URL") }))
	for _, path := range []string{"/health/live", "/health/ready"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if path == "/health/live" {
			if w.Code != 200 || calls != 0 {
				t.Fatal("limiter broke liveness")
			}
		} else if w.Code != 503 || !strings.Contains(w.Body.String(), "limiter_unavailable") || strings.Contains(w.Body.String(), "private") {
			t.Fatal("unsafe readiness", w.Body.String())
		}
	}
}
