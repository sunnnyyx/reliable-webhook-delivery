package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"relayforge/internal/telemetry"
	"time"
)

type Pinger interface{ Ping(context.Context) error }

func Health(db Pinger, timeout time.Duration, limiter ...Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { write(w, r, 200, "live", "") })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if db.Ping(ctx) != nil {
			write(w, r, 503, "", "storage_unavailable")
			return
		}
		for _, p := range limiter {
			if p.Ping(ctx) != nil {
				write(w, r, 503, "", "limiter_unavailable")
				return
			}
		}
		write(w, r, 200, "ready", "")
	})
	return withRequestID(mux)
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			http.Error(w, "request ID unavailable", 500)
			return
		}
		requestID := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", requestID)
		r = r.WithContext(telemetry.WithRequestID(r.Context(), requestID))
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func write(w http.ResponseWriter, r *http.Request, status int, state, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"request_id": w.Header().Get("X-Request-ID")}
	if code != "" {
		body["code"] = code
		body["message"] = "durable storage is unavailable"
		if code == "limiter_unavailable" {
			body["message"] = "outbound rate limiter is unavailable"
		}
	} else {
		body["status"] = state
	}
	_ = json.NewEncoder(w).Encode(body)
}
