package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"strings"
	"time"
)

type Accounts interface {
	WebCredential(context.Context, string) (postgres.Credential, error)
	DeleteBrowserKey(context.Context, string) error
	MintWebKey(context.Context, string) (postgres.Credential, error)
	WebKeys(context.Context, string) ([]postgres.WebKey, error)
	RevokeWebKey(context.Context, string, string) error
	RevokeWebSessions(context.Context, string) error
}
type Identity struct {
	origin, key string
	client      *http.Client
}

func NewIdentity(origin, key string) (*Identity, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || !strings.HasPrefix(key, "sb_publishable_") || len(key) < 20 {
		return nil, errors.New("configure a Supabase HTTPS origin and publishable key")
	}
	if !strings.HasSuffix(u.Hostname(), ".supabase.co") {
		return nil, errors.New("identity origin must be a Supabase project")
	}
	return &Identity{origin, key, &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 8, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (i *Identity) call(ctx context.Context, method, path, token string, input any) (map[string]json.RawMessage, int, error) {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, i.origin+"/auth/v1"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("apikey", i.key)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return nil, 0, err
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(data, &result) != nil {
		return nil, resp.StatusCode, errors.New("invalid identity response")
	}
	return result, resp.StatusCode, nil
}
func field(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

// The remote user endpoint is authoritative; unverified JWT payloads and
// client-provided tenant/user IDs never decide workspace ownership.
func (i *Identity) user(ctx context.Context, token string) (string, error) {
	data, status, err := i.call(ctx, "GET", "/user", token, nil)
	if err != nil || status != 200 || !security.ValidID(field(data, "id")) || field(data, "email_confirmed_at") == "" {
		return "", errors.New("verified identity required")
	}
	var identities []struct {
		Provider string `json:"provider"`
	}
	if json.Unmarshal(data["identities"], &identities) != nil {
		return "", errors.New("GitHub identity required")
	}
	for _, identity := range identities {
		if identity.Provider == "github" {
			return field(data, "id"), nil
		}
	}
	return "", errors.New("GitHub identity required")
}
func NewHosted(address, apiURL, origin string, identity *Identity, accounts Accounts) (*Server, error) {
	// Keep the API private on loopback even when the console is publicly proxied.
	s, err := New(address, apiURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || identity == nil || accounts == nil {
		s.Close()
		return nil, errors.New("hosted console requires an HTTPS public origin and identity configuration")
	}
	s.origin = origin
	s.secure = true
	s.identity = identity
	s.accounts = accounts
	s.authSlots = make(chan struct{}, 4)
	return s, nil
}

type oauthFlow struct {
	verifier string
	expires  time.Time
}

const flowCookie = "relayforge_oauth"

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	callback := r.Method == "GET" && r.URL.Path == "/auth/callback"
	if !callback && (r.Method != "POST" || r.URL.Path != "/auth/github" || r.URL.RawQuery != "") {
		fail(w, 404, "Use GitHub sign-in.")
		return
	}
	s.mu.Lock()
	now := s.now()
	if now.Sub(s.loginWindow) >= time.Minute {
		s.loginWindow = now
		s.loginCount = 0
	}
	s.loginCount++
	limited := s.loginCount > 60
	if s.flows == nil {
		s.flows = make(map[[32]byte]oauthFlow)
	}
	for k, v := range s.flows {
		if !now.Before(v.expires) {
			delete(s.flows, k)
		}
	}
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "Too many sign-in attempts. Try again shortly.")
		return
	}
	select {
	case s.authSlots <- struct{}{}:
		defer func() { <-s.authSlots }()
	default:
		fail(w, 429, "Try again shortly.")
		return
	}
	if !callback {
		token, verifier := random(), random()
		s.mu.Lock()
		if len(s.flows) >= maxSessions {
			s.mu.Unlock()
			fail(w, 503, "Sign-in capacity reached.")
			return
		}
		if old, e := r.Cookie(flowCookie); e == nil {
			delete(s.flows, sha256.Sum256([]byte(old.Value)))
		}
		s.flows[sha256.Sum256([]byte(token))] = oauthFlow{verifier, now.Add(5 * time.Minute)}
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: token, Path: "/auth/callback", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 300})
		hash := sha256.Sum256([]byte(verifier))
		q := url.Values{"provider": {"github"}, "redirect_to": {s.origin + "/auth/callback"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"s256"}}
		write(w, 200, map[string]string{"url": s.identity.origin + "/auth/v1/authorize?" + q.Encode()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/callback", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	cookie, err := r.Cookie(flowCookie)
	if err != nil || len(cookie.Value) != 43 {
		fail(w, 400, "Sign-in expired. Return to the app and try again.")
		return
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	s.mu.Lock()
	flow, ok := s.flows[hash]
	delete(s.flows, hash)
	s.mu.Unlock()
	codes := r.URL.Query()["code"]
	if !ok || !now.Before(flow.expires) || len(codes) != 1 || len(codes[0]) < 1 || len(codes[0]) > 2048 || r.URL.Query().Get("error") != "" {
		fail(w, 400, "Sign-in expired or cancelled. Return to the app and try again.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	data, status, err := s.identity.call(ctx, "POST", "/token?grant_type=pkce", "", map[string]string{"auth_code": codes[0], "code_verifier": flow.verifier})
	if err != nil || status != 200 {
		fail(w, 401, "GitHub sign-in failed. Return to the app and try again.")
		return
	}
	user, err := s.identity.user(ctx, field(data, "access_token"))
	if err != nil {
		fail(w, 401, "A verified GitHub identity is required.")
		return
	}
	c, err := s.accounts.WebCredential(ctx, user)
	if err != nil {
		fail(w, 503, "Workspace unavailable. Try again later.")
		return
	}
	// Create the opaque session without returning credentials or provider tokens.
	out := sessionHeaders{header: make(http.Header)}
	s.startSession(&out, r, c.APIKey)
	for _, v := range out.header.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", v)
	}
	if out.status != 200 {
		fail(w, 503, "Session unavailable. Try again.")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Capture the existing session helper's headers while suppressing its JSON body
// for the OAuth redirect. No provider or application tokens reach browser JS.
type sessionHeaders struct {
	header http.Header
	status int
}

func (w *sessionHeaders) Header() http.Header         { return w.header }
func (w *sessionHeaders) WriteHeader(code int)        { w.status = code }
func (w *sessionHeaders) Write(b []byte) (int, error) { return len(b), nil }

// NewLocalGitHub supports provider testing only on the existing exact loopback origin.
func NewLocalGitHub(address, apiURL string, identity *Identity, accounts Accounts) (*Server, error) {
	if identity == nil || accounts == nil {
		return nil, errors.New("GitHub identity configuration required")
	}
	s, err := New(address, apiURL)
	if err != nil {
		return nil, err
	}
	s.identity = identity
	s.accounts = accounts
	s.authSlots = make(chan struct{}, 4)
	return s, nil
}
