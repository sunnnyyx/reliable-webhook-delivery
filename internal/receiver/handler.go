package receiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"relayforge/internal/events"
	"relayforge/internal/jsonutil"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"time"
)

// Handler authenticates raw bytes before touching receiver-owned transactional storage.
func Handler(keys [][]byte, log *slog.Logger, now func() time.Time, processor Processor) http.Handler {
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ready, ok := processor.(interface{ Ready(context.Context) error })
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if !ok || ready.Ready(ctx) != nil {
			http.Error(w, "receiver storage unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("POST /webhooks", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, events.MaxInputBytes+512)
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			http.Error(w, "invalid request body", 413)
			return
		}
		for _, name := range []string{"webhook-id", "webhook-timestamp", "webhook-signature"} {
			if len(r.Header.Values(name)) != 1 {
				http.Error(w, "invalid signature metadata", 401)
				return
			}
		}
		id := r.Header.Get("webhook-id")
		if !security.ValidID(id) || signing.Verify(id, r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), raw, keys, now(), 5*time.Minute) != nil {
			http.Error(w, "invalid signature", 401)
			return
		}
		var envelope Envelope
		var fields map[string]json.RawMessage
		if jsonutil.Validate(raw) != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || fields["id"] == nil || fields["type"] == nil || fields["created_at"] == nil || fields["data"] == nil || json.Unmarshal(raw, &envelope) != nil || !security.ValidID(envelope.ID) || !events.ValidType(envelope.Type) || envelope.CreatedAt.IsZero() || len(envelope.Data) == 0 {
			http.Error(w, "invalid envelope", 400)
			return
		}
		if processor == nil {
			http.Error(w, "receiver storage unavailable", 503)
			return
		}
		bounded, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		duplicate, e := processor.Process(bounded, id, envelope, raw)
		if errors.Is(e, ErrEventConflict) {
			http.Error(w, "event identity conflict", 409)
			return
		}
		if e != nil {
			log.Error("receiver persistence failed", "error_code", "storage_unavailable")
			http.Error(w, "receiver storage unavailable", 503)
			return
		}
		log.Info("webhook acknowledged", "delivery_id", id, "event_id", envelope.ID, "duplicate", duplicate)
		w.WriteHeader(204)
	})
	return mux
}
