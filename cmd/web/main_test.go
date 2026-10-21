package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPublicGatewayRoutesAndReadiness(t *testing.T) {
	ready := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/ready" {
			if !ready {
				w.WriteHeader(503)
			}
			return
		}
		w.Header().Set("X-Upstream", "api")
		w.WriteHeader(202)
	}))
	defer api.Close()
	ui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "console")
		w.WriteHeader(200)
	}))
	defer ui.Close()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer worker.Close()
	a, _ := url.Parse(api.URL)
	u, _ := url.Parse(ui.URL)
	gateway := newGateway("public.example", a, u, worker.URL)
	for _, tc := range []struct {
		path, host, upstream string
		status               int
	}{{"/v1/events", "public.example", "api", 202}, {"/auth/login", "public.example", "console", 200}, {"/", "evil.example", "", 403}, {"/health/ready", "internal-probe", "", 200}} {
		req := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
		out := httptest.NewRecorder()
		gateway.ServeHTTP(out, req)
		if out.Code != tc.status || out.Header().Get("X-Upstream") != tc.upstream {
			t.Fatalf("%s route: %d", tc.path, out.Code)
		}
	}
	ready = false
	out := httptest.NewRecorder()
	gateway.ServeHTTP(out, httptest.NewRequest("GET", "http://public.example/health/ready", nil))
	if out.Code != 503 {
		t.Fatal("unready API was hidden")
	}
}
