package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"relayforge/internal/repository/postgres"
	"strings"
	"testing"
	"time"
)

type fakeAccounts struct {
	users   []string
	deleted int
	resets  int
}

func (f *fakeAccounts) WebCredential(_ context.Context, id string) (postgres.Credential, error) {
	f.users = append(f.users, id)
	return postgres.Credential{APIKey: testKey}, nil
}
func (f *fakeAccounts) DeleteBrowserKey(context.Context, string) error { f.deleted++; return nil }
func (f *fakeAccounts) MintWebKey(context.Context, string) (postgres.Credential, error) {
	return postgres.Credential{}, nil
}
func (f *fakeAccounts) WebKeys(context.Context, string) ([]postgres.WebKey, error) { return nil, nil }
func (f *fakeAccounts) RevokeWebKey(context.Context, string, string) error         { return nil }
func (f *fakeAccounts) RevokeWebSessions(context.Context, string) error            { f.resets++; return nil }
func hostedFixture(t *testing.T, confirmed bool) (*Server, *fakeAccounts) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("apikey") != "publishable-test-key" {
			t.Error("missing provider API key")
		}
		if r.URL.Path == "/auth/v1/user" {
			if r.Header.Get("Authorization") != "Bearer verified-test-token" {
				t.Error("token not verified remotely")
			}
			if r.Method == "PUT" {
				_, _ = w.Write([]byte(`{"id":"10000000-0000-4000-8000-000000000001"}`))
				return
			}
			date := ""
			if confirmed {
				date = "2026-10-17T00:00:00Z"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "10000000-0000-4000-8000-000000000001", "email_confirmed_at": date, "identities": []map[string]string{{"provider": "github"}}})
			return
		}
		if r.URL.Path != "/auth/v1/token" || r.URL.Query().Get("grant_type") != "pkce" {
			t.Error("unexpected identity endpoint")
		}
		var exchange map[string]string
		_ = json.NewDecoder(r.Body).Decode(&exchange)
		if exchange["auth_code"] != "provider-code" || len(exchange["code_verifier"]) != 43 {
			t.Error("missing code or verifier")
		}
		_, _ = w.Write([]byte(`{"access_token":"verified-test-token"}`))
	}))
	t.Cleanup(provider.Close)
	identity := &Identity{provider.URL, "publishable-test-key", provider.Client()}
	accounts := &fakeAccounts{}
	s, e := NewHosted("127.0.0.1:8091", "http://127.0.0.1:8080", "https://example.com", identity, accounts)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s, accounts
}
func hostedSend(s *Server, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://example.com"+path, strings.NewReader(body))
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func oauthStart(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := hostedSend(s, "/auth/github", `{}`)
	if w.Code != 200 {
		t.Fatalf("start %d", w.Code)
	}
	var data map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	u, e := url.Parse(data["url"])
	if e != nil || u.Query().Get("provider") != "github" || u.Query().Get("code_challenge_method") != "s256" || u.Query().Get("redirect_to") != "https://example.com/auth/callback" {
		t.Fatal("unsafe authorization URL")
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe OAuth cookie")
	}
	s.mu.Lock()
	flow := s.flows[sha256.Sum256([]byte(cookie.Value))]
	s.mu.Unlock()
	challenge := sha256.Sum256([]byte(flow.verifier))
	if u.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) {
		t.Fatal("PKCE mismatch")
	}
	return cookie, flow.verifier
}
func oauthCallback(s *Server, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://example.com/auth/callback?code=provider-code", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestHostedGitHubCallbackAndLogout(t *testing.T) {
	s, a := hostedFixture(t, true)
	c, _ := oauthStart(t, s)
	w := oauthCallback(s, c)
	if w.Code != 303 || w.Header().Get("Location") != "/" || len(a.users) != 1 {
		t.Fatalf("callback %d", w.Code)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == cookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.Secure || !sessionCookie.HttpOnly || strings.Contains(w.Body.String(), testKey) {
		t.Fatal("unsafe session")
	}
	req := httptest.NewRequest("GET", "https://example.com/session", nil)
	req.AddCookie(sessionCookie)
	out := httptest.NewRecorder()
	s.ServeHTTP(out, req)
	var data struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(out.Body.Bytes(), &data)
	req = httptest.NewRequest("DELETE", "https://example.com/session", nil)
	req.AddCookie(sessionCookie)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("X-CSRF-Token", data.CSRF)
	out = httptest.NewRecorder()
	s.ServeHTTP(out, req)
	if out.Code != 204 || a.deleted != 1 {
		t.Fatal("logout failed")
	}
	if oauthCallback(s, c).Code != 400 {
		t.Fatal("OAuth replay accepted")
	}
}
func TestHostedRejectsUnverifiedIdentityAndForeignOrigin(t *testing.T) {
	s, a := hostedFixture(t, false)
	c, _ := oauthStart(t, s)
	if oauthCallback(s, c).Code != 401 || len(a.users) != 0 {
		t.Fatal("unverified account accepted")
	}
	if oauthCallback(s, nil).Code != 400 {
		t.Fatal("missing flow cookie accepted")
	}
	r := httptest.NewRequest("POST", "https://example.com/auth/github", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
	for _, path := range []string{"/auth/login", "/auth/signup", "/auth/reset", "/session"} {
		if hostedSend(s, path, `{}`).Code != 404 {
			t.Fatal("obsolete login route enabled")
		}
	}
}
func TestOAuthFlowExpiry(t *testing.T) {
	s, a := hostedFixture(t, true)
	c, _ := oauthStart(t, s)
	now := s.now()
	s.now = func() time.Time { return now.Add(6 * time.Minute) }
	if oauthCallback(s, c).Code != 400 || len(a.users) != 0 {
		t.Fatal("expired flow accepted")
	}
}
func TestHostedConfigBounds(t *testing.T) {
	s, a := hostedFixture(t, true)
	if _, err := NewHosted("127.0.0.1:8091", "http://127.0.0.1:8080", "http://example.com", s.identity, a); err == nil {
		t.Fatal("insecure origin accepted")
	}
	if _, err := NewIdentity("https://example.com", "publishable-test-key"); err == nil {
		t.Fatal("arbitrary provider accepted")
	}
	if _, err := NewIdentity("https://test.supabase.co", "sb_secret_not-for-browser"); err == nil {
		t.Fatal("service key accepted")
	}
}

func TestIdentityRequiresGitHubProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"10000000-0000-4000-8000-000000000001","email_confirmed_at":"2026-10-21T00:00:00Z","identities":[{"provider":"email"}]}`))
	}))
	defer upstream.Close()
	i := &Identity{upstream.URL, "test-key", upstream.Client()}
	if _, err := i.user(context.Background(), "test-token"); err == nil {
		t.Fatal("non-GitHub identity accepted")
	}
}
