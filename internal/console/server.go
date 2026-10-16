// Package console provides a loopback-only browser BFF. API credentials live in
// bounded process memory; the browser receives only an opaque short-lived cookie.
package console

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/*
var assets embed.FS

const cookieName = "relayforge_console"
const lifetime = 30 * time.Minute
const maxSessions = 128
const maxBody = 256 << 10
const maxResponse = 2 << 20

type session struct {
	key, csrf string
	expires   time.Time
}
type Server struct {
	origin      string
	upstream    *url.URL
	client      *http.Client
	mu          sync.Mutex
	sessions    map[[32]byte]session
	loginWindow time.Time
	loginCount  int
	now         func() time.Time
}

// New deliberately supports local development only. Remote production hosting
// needs a separate TLS/identity design; neither listener nor upstream can expand
// to arbitrary destinations through request input or forwarded headers.
func New(address, apiURL string) (*Server, error) {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return nil, errors.New("console address must use literal loopback")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("invalid console port")
	}
	u, err := url.Parse(apiURL)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return nil, errors.New("console API URL must be a loopback HTTP origin")
	}
	apiIP := net.ParseIP(u.Hostname())
	if apiIP == nil || !apiIP.IsLoopback() || u.Port() == "" {
		return nil, errors.New("console API URL must use literal loopback and a port")
	}
	if n, err = strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return nil, errors.New("invalid API port")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	return &Server{origin: "http://" + address, upstream: u, client: &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, sessions: make(map[[32]byte]session), now: time.Now}, nil
}
func (s *Server) Close() {
	s.client.CloseIdleConnections()
	s.mu.Lock()
	clear(s.sessions)
	s.mu.Unlock()
}
func random() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]string{"message": message})
}
func (s *Server) get(r *http.Request) (session, [32]byte, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 43 {
		return session{}, [32]byte{}, false
	}
	hash := sha256.Sum256([]byte(c.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[hash]
	if ok && !s.now().Before(v.expires) {
		delete(s.sessions, hash)
		ok = false
	}
	return v, hash, ok
}
func (s *Server) remove(w http.ResponseWriter, hash [32]byte) {
	s.mu.Lock()
	delete(s.sessions, hash)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	if "http://"+r.Host != s.origin {
		fail(w, 403, "Use the configured console loopback address.")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		fail(w, 403, "Cross-site access denied.")
		return
	}
	mutation := r.Method != "GET" && r.Method != "HEAD"
	if mutation && (len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.origin) {
		fail(w, 403, "Same-origin request required.")
		return
	}
	if r.URL.Path == "/session" && r.Method == "POST" {
		s.login(w, r)
		return
	}
	if r.URL.Path == "/session" && r.Method == "GET" {
		v, _, ok := s.get(r)
		if !ok {
			fail(w, 401, "Connect with a tenant API key.")
			return
		}
		write(w, 200, map[string]any{"csrf": v.csrf, "expires_at": v.expires})
		return
	}
	if r.URL.Path == "/session" && r.Method == "DELETE" || strings.HasPrefix(r.URL.Path, "/api/") {
		v, hash, ok := s.get(r)
		if !ok {
			fail(w, 401, "Session expired. Connect again.")
			return
		}
		if mutation && (len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(v.csrf)) != 1) {
			fail(w, 403, "Refresh the page and try again.")
			return
		}
		if r.URL.Path == "/session" {
			s.remove(w, hash)
			w.WriteHeader(204)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api")
		if r.URL.RawPath != "" || !allowed(r.Method, path) {
			fail(w, 404, "Route unavailable.")
			return
		}
		if mutation && r.Header.Get("Content-Type") != "application/json" {
			fail(w, 415, "Use application/json.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(w, 413, "Request exceeds 256 KiB.")
			return
		}
		response, raw, err := s.call(r, path, v.key, body)
		if err != nil {
			fail(w, 502, "API unavailable. Retry with the same idempotency key.")
			return
		}
		if response.StatusCode == 401 {
			s.remove(w, hash)
		}
		for _, name := range []string{"X-Request-ID", "X-Trace-ID", "Idempotency-Replayed", "Retry-After"} {
			if value := response.Header.Get(name); value != "" {
				h.Set(name, value)
			}
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			fail(w, 502, "Unexpected API redirect.")
			return
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		fail(w, 405, "Method unavailable.")
		return
	}
	file := map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css", "/favicon.svg": "favicon.svg"}[r.URL.Path]
	if file == "" {
		fail(w, 404, "Page unavailable.")
		return
	}
	data, _ := assets.ReadFile("assets/" + file)
	mime := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8", "favicon.svg": "image/svg+xml"}[file]
	h.Set("Content-Type", mime)
	w.WriteHeader(200)
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}

var resource = regexp.MustCompile(`^/v1/(endpoints|events|deliveries)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var action = regexp.MustCompile(`^/v1/(endpoints|deliveries)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/(rotate-secret|attempts|replay|replays)$`)

func allowed(method, path string) bool {
	if path == "/v1/endpoints" || path == "/v1/events" {
		return method == "GET" || method == "POST"
	}
	if path == "/v1/deliveries" {
		return method == "GET"
	}
	if resource.MatchString(path) {
		return method == "GET" || strings.HasPrefix(path, "/v1/endpoints/") && (method == "PATCH" || method == "DELETE")
	}
	if action.MatchString(path) {
		if strings.HasPrefix(path, "/v1/endpoints/") {
			return strings.HasSuffix(path, "/rotate-secret") && method == "POST"
		}
		return method == "GET" && (strings.HasSuffix(path, "/attempts") || strings.HasSuffix(path, "/replays")) || method == "POST" && strings.HasSuffix(path, "/replay")
	}
	return false
}
func (s *Server) call(r *http.Request, path, key string, body []byte) (*http.Response, []byte, error) {
	u := *s.upstream
	u.Path = path
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return nil, nil, errors.New("invalid API response")
	}
	if response.StatusCode != 204 && !json.Valid(raw) {
		return nil, nil, errors.New("invalid API response")
	}
	return response, raw, nil
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	now := s.now()
	if now.Sub(s.loginWindow) >= time.Minute {
		s.loginWindow = now
		s.loginCount = 0
	}
	s.loginCount++
	limited := s.loginCount > 10
	for k, v := range s.sessions {
		if !now.Before(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "Too many connection attempts. Wait one minute.")
		return
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		fail(w, 400, "Supply a JSON API key.")
		return
	}
	var input struct {
		Key string `json:"key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || len(input.Key) < 16 || len(input.Key) > 512 || strings.ContainsAny(input.Key, " \r\n\t") {
		fail(w, 400, "Supply a valid API key.")
		return
	}
	// Validate through the existing API: revocation/tenant checks remain authoritative.
	check := r.Clone(r.Context())
	check.Method = "GET"
	check.URL = &url.URL{}
	response, _, err := s.call(check, "/v1/endpoints", input.Key, nil)
	if err != nil {
		fail(w, 502, "API unavailable. Try again.")
		return
	}
	if response.StatusCode != 200 {
		if response.StatusCode == 401 {
			fail(w, 401, "API key is invalid or revoked.")
		} else {
			fail(w, 503, "API is not ready. Try again.")
		}
		return
	}
	token := random()
	v := session{key: input.Key, csrf: random(), expires: now.Add(lifetime)}
	s.mu.Lock()
	if len(s.sessions) >= maxSessions {
		s.mu.Unlock()
		fail(w, 503, "Console session capacity reached.")
		return
	}
	s.sessions[sha256.Sum256([]byte(token))] = v
	s.mu.Unlock()
	// A repeated login rotates and invalidates this browser's prior session.
	if _, hash, ok := s.get(r); ok {
		s.mu.Lock()
		delete(s.sessions, hash)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(lifetime.Seconds())})
	write(w, 200, map[string]any{"csrf": v.csrf, "expires_at": v.expires})
}
