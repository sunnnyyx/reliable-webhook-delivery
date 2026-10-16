package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "rf_test_only_credential_0123456789"

func fixture(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	api := httptest.NewServer(upstream)
	t.Cleanup(api.Close)
	s, e := New("127.0.0.1:8091", api.URL)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}
func send(s *Server, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8091"+path, strings.NewReader(body))
	r.Header.Set("Origin", "http://127.0.0.1:8091")
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func loginTest(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := send(s, "POST", "/session", `{"key":"`+testKey+`"}`, nil, "")
	if w.Code != 200 {
		t.Fatalf("login status %d", w.Code)
	}
	var data struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 1800 || strings.Contains(w.Body.String(), testKey) || len(data.CSRF) != 43 {
		t.Fatal("unsafe session")
	}
	return cookies[0], data.CSRF
}
func TestSessionProxyAndCSRF(t *testing.T) {
	var mu sync.Mutex
	var calls int
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("API credentials missing")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("Baggage") != "" {
			t.Error("browser headers leaked")
		}
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"type":"a","data":{}}` || r.Header.Get("Idempotency-Key") != "retry-this" {
				t.Error("idempotent bytes changed")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=unsafe")
		w.Header().Set("X-Request-ID", "request-fixture")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	cookie, csrf := loginTest(t, s)
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		w := send(s, method, "/api/v1/endpoints", "{}", cookie, "")
		if w.Code != 403 {
			t.Fatalf("missing CSRF %s: %d", method, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8091/api/v1/events", bytes.NewBufferString(`{"type":"a","data":{}}`))
	r.AddCookie(cookie)
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", csrf)
	r.Header.Set("Idempotency-Key", "retry-this")
	r.Header.Set("Baggage", "private")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Request-ID") != "request-fixture" {
		t.Fatal("unsafe proxy response")
	}
	mu.Lock()
	if calls != 2 {
		t.Errorf("unexpected upstream calls %d", calls)
	}
	mu.Unlock()
	if send(s, "DELETE", "/session", "", cookie, csrf).Code != 204 || send(s, "GET", "/api/v1/events", "", cookie, "").Code != 401 {
		t.Fatal("logout failed")
	}
}
func TestOriginHostAndRouteBoundaries(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) })
	cookie, csrf := loginTest(t, s)
	for _, tc := range []struct{ method, path string }{{"POST", "/api/v1/health/ready"}, {"GET", "/api/v1/deliveries/../endpoints"}, {"GET", "/api/v1/endpoints/evil"}, {"GET", "/api/v1/endpoints/12345678-1234-1234-1234-123456789012/attempts"}, {"POST", "/api/v1/deliveries"}} {
		if w := send(s, tc.method, tc.path, "{}", cookie, csrf); w.Code != 404 {
			t.Errorf("route %s: %d", tc.path, w.Code)
		}
	}
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.Host = "attacker.example:8091" }, func(r *http.Request) { r.Header.Set("Origin", "http://attacker.example") }, func(r *http.Request) { r.Header.Del("Origin") }, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }} {
		r := httptest.NewRequest("POST", s.origin+"/session", strings.NewReader(`{}`))
		r.Header.Set("Origin", s.origin)
		mutate(r)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("origin boundary bypass")
		}
	}
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := send(s, "GET", path, "", nil, "")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || strings.Contains(w.Body.String(), testKey) {
			t.Fatal("asset security headers")
		}
	}
}
func TestExpiryRevocationRotationAndBounds(t *testing.T) {
	revoked := false
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if revoked {
			w.WriteHeader(401)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	cookie, csrf := loginTest(t, s)
	other, _ := loginTest(t, s)
	// Different browser sessions do not invalidate one another.
	if send(s, "GET", "/session", "", cookie, "").Code != 200 {
		t.Fatal("session isolation")
	}
	replacement := send(s, "POST", "/session", `{"key":"`+testKey+`"}`, cookie, "")
	if replacement.Code != 200 || send(s, "GET", "/session", "", cookie, "").Code != 401 {
		t.Fatal("rotation failed")
	}
	revoked = true
	if send(s, "GET", "/api/v1/events", "", other, "").Code != 401 || send(s, "GET", "/session", "", other, "").Code != 401 {
		t.Fatal("revoked key session retained")
	}
	revoked = false
	cookie, csrf = loginTest(t, s)
	if send(s, "POST", "/api/v1/events", strings.Repeat("a", maxBody+1), cookie, csrf).Code != 413 {
		t.Fatal("request limit")
	}
	s.now = func() time.Time { return time.Now().Add(lifetime + time.Second) }
	if send(s, "GET", "/session", "", cookie, "").Code != 401 {
		t.Fatal("expired session accepted")
	}
}
func TestLoginThrottleAndSessionCapacity(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) })
	for i := 0; i < 10; i++ {
		if send(s, "POST", "/session", `{}`, nil, "").Code != 400 {
			t.Fatal("invalid input")
		}
	}
	if send(s, "POST", "/session", `{}`, nil, "").Code != 429 {
		t.Fatal("login throttle missing")
	}
	s.loginCount = 0
	for i := 0; i < maxSessions; i++ {
		s.sessions[shaHash(i)] = session{expires: time.Now().Add(time.Hour)}
	}
	if send(s, "POST", "/session", `{"key":"`+testKey+`"}`, nil, "").Code != 503 {
		t.Fatal("unbounded sessions")
	}
}
func shaHash(i int) [32]byte { var b [32]byte; b[0] = byte(i); return b }
func TestUpstreamFailuresAreSanitized(t *testing.T) {
	for _, mode := range []string{"redirect", "invalid", "huge"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					w.Header().Set("Location", "http://example.com/private")
					w.WriteHeader(302)
				case "invalid":
					_, _ = w.Write([]byte("private internal diagnostic"))
				case "huge":
					_, _ = w.Write([]byte(strings.Repeat("x", maxResponse+1)))
				}
			})
			w := send(s, "POST", "/session", `{"key":"`+testKey+`"}`, nil, "")
			if w.Code < 500 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), testKey) {
				t.Fatal("upstream failure exposed")
			}
		})
	}
}
func TestConfigurationRemainsLocal(t *testing.T) {
	for _, tc := range [][2]string{{"0.0.0.0:8091", "http://127.0.0.1:8080"}, {"localhost:8091", "http://127.0.0.1:8080"}, {"127.0.0.1:8091", "http://example.com:8080"}, {"127.0.0.1:8091", "http://127.0.0.1:8080/v1"}, {"127.0.0.1:8091", "http://secret@127.0.0.1:8080"}, {"127.0.0.1:8091", "http://127.0.0.1:8080?secret=yes"}} {
		if s, e := New(tc[0], tc[1]); e == nil {
			s.Close()
			t.Fatal("unsafe configuration accepted")
		}
	}
}
