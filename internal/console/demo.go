package console

import (
	"errors"
	"net/http"
	"net/url"
	"relayforge/internal/playground"
	"time"
)

// NewDemo exposes only an ephemeral sandbox, never the configured real API.
func NewDemo(address, publicOrigin string) (*Server, error) {
	s, err := New(address, "http://127.0.0.1:1")
	if err != nil {
		return nil, err
	}
	if publicOrigin != "" {
		u, e := url.Parse(publicOrigin)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			s.Close()
			return nil, errors.New("PUBLIC_URL must be an HTTPS origin")
		}
		s.origin = publicOrigin
		s.secure = true
	}
	s.demo, err = playground.New()
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Server) demoSession(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.get(r); ok {
		v, _, _ := s.get(r)
		write(w, 200, map[string]any{"csrf": v.csrf, "expires_at": v.expires})
		return
	}
	s.mu.Lock()
	now := s.now()
	if now.Sub(s.loginWindow) >= time.Minute {
		s.loginWindow = now
		s.loginCount = 0
	}
	s.loginCount++
	limited := s.loginCount > 20
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "Demo is busy. Try again in one minute.")
		return
	}
	key, err := s.demo.Create()
	if err != nil {
		fail(w, 503, "Demo capacity reached. Try again later.")
		return
	}
	s.startSession(w, r, key)
}
