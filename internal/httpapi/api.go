package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"strconv"
	"strings"
	"time"
)

type tenantKey struct{}

func API(store *postgres.Store, timeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/health/", Health(store, timeout))
	routes := http.NewServeMux()
	registerEvents(routes, store)
	routes.HandleFunc("POST /v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		var input endpoints.Create
		if !decode(w, r, &input) {
			return
		}

		v, secret, e := store.CreateEndpoint(r.Context(), tenant(r), input)
		if failure(w, e) {
			return
		}
		w.Header().Set("Location", "/v1/endpoints/"+v.ID)
		respond(w, 201, map[string]any{"endpoint": v, "signing_secret": secret})
	})
	routes.HandleFunc("GET /v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		limit := 25
		query, parseErr := url.ParseQuery(r.URL.RawQuery)
		if parseErr != nil {
			apiError(w, 400, "invalid_query", "malformed query string")
			return
		}
		for k, v := range query {
			if (k != "limit" && k != "cursor") || len(v) != 1 {
				apiError(w, 400, "invalid_query", "invalid pagination query")
				return
			}
		}
		if raw, ok := query["limit"]; ok {
			n, e := strconv.Atoi(raw[0])
			if e != nil || n < 1 || n > 100 {
				apiError(w, 400, "invalid_query", "limit must be 1–100")
				return
			}
			limit = n
		}
		var cursor *endpoints.Cursor
		if raw, ok := query["cursor"]; ok {
			var e error
			cursor, e = endpoints.DecodeCursor(raw[0], tenant(r))
			if e != nil {
				apiError(w, 400, "invalid_cursor", "invalid cursor")
				return
			}
		}
		items, next, e := store.ListEndpoints(r.Context(), tenant(r), limit, cursor)
		if failure(w, e) {
			return
		}
		respond(w, 200, map[string]any{"data": items, "next_cursor": next})
	})
	routes.HandleFunc("GET /v1/endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, e := store.GetEndpoint(r.Context(), tenant(r), id)
		if failure(w, e) {
			return
		}
		respond(w, 200, v)
	})
	routes.HandleFunc("PATCH /v1/endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var input endpoints.Patch
		if !decode(w, r, &input) {
			return
		}
		if input.URL == nil && input.Enabled == nil && input.Subscriptions == nil {
			apiError(w, 400, "invalid_input", "supply a URL, enabled flag, or subscriptions")
			return
		}
		v, e := store.UpdateEndpoint(r.Context(), tenant(r), id, input)
		if failure(w, e) {
			return
		}
		respond(w, 200, v)
	})
	routes.HandleFunc("DELETE /v1/endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if failure(w, store.DeleteEndpoint(r.Context(), tenant(r), id)) {
			return
		}
		w.WriteHeader(204)
	})
	routes.HandleFunc("POST /v1/endpoints/{id}/rotate-secret", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
		secret, e := store.RotateSecret(r.Context(), tenant(r), id)
		if failure(w, e) {
			return
		}
		respond(w, 201, map[string]string{"signing_secret": secret})
	})
	routes.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "not_found", "resource not found") })
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			unauthorized(w)
			return
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		id, e := store.Authenticate(ctx, parts[1])
		if errors.Is(e, endpoints.ErrNotFound) {
			unauthorized(w)
			return
		}
		if failure(w, e) {
			return
		}
		routes.ServeHTTP(w, r.WithContext(context.WithValue(ctx, tenantKey{}, id)))
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "not_found", "resource not found") })
	return withRequestID(store.Telemetry.HTTP(mux))
}
func tenant(r *http.Request) string { return r.Context().Value(tenantKey{}).(string) }
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !security.ValidID(id) {
		apiError(w, 404, "not_found", "resource not found")
		return "", false
	}
	return id, true
}
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	apiError(w, 401, "unauthorized", "valid bearer credentials required")
}
func failure(w http.ResponseWriter, e error) bool {
	if e == nil {
		return false
	}
	var validation postgres.ValidationError
	switch {
	case errors.Is(e, postgres.ErrBacklog):
		w.Header().Set("Retry-After", "1")
		apiError(w, 429, "backlog_limit", "tenant live delivery backlog limit reached; retry the same idempotency key after capacity becomes available")
	case errors.Is(e, delivery.ErrReplayState):
		apiError(w, 409, "replay_unavailable", "replay requires dead-letter work, an enabled endpoint, and an unexpired event")
	case errors.Is(e, events.ErrIdempotencyConflict):
		apiError(w, 409, "idempotency_mismatch", "Idempotency-Key was already used with different request bytes")
	case errors.Is(e, endpoints.ErrNotFound):
		apiError(w, 404, "not_found", "resource not found")
	case errors.Is(e, endpoints.ErrLimit):
		apiError(w, 409, "endpoint_limit", "tenant endpoint limit reached")
	case errors.Is(e, endpoints.ErrOverlap):
		apiError(w, 409, "rotation_overlap", "previous rotation overlap is active")
	case errors.As(e, &validation):
		apiError(w, 400, "invalid_input", validation.Error())
	default:
		apiError(w, 503, "storage_unavailable", "durable storage is unavailable")
	}
	return true
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]string{"code": code, "message": message, "request_id": w.Header().Get("X-Request-ID")})
}
func readJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	kind, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || kind != "application/json" {
		apiError(w, 400, "invalid_input", "Content-Type must be application/json")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, events.MaxInputBytes)
	b, e := io.ReadAll(r.Body)
	if e != nil {
		var max *http.MaxBytesError
		if errors.As(e, &max) {
			apiError(w, 413, "payload_too_large", "request exceeds 256 KiB")
		} else {
			apiError(w, 400, "invalid_input", "cannot read request")
		}
		return nil, false
	}
	if e = validateJSON(b); e != nil {
		apiError(w, 400, "invalid_input", "supply one JSON object with unique keys")
		return nil, false
	}
	return b, true
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	b, ok := readJSON(w, r)
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	allowed := map[string]bool{}
	typ := reflect.TypeOf(target).Elem()
	for i := 0; i < typ.NumField(); i++ {
		allowed[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for k, v := range fields {
		if !allowed[k] || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			apiError(w, 400, "invalid_input", "unknown or null JSON field")
			return false
		}
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(target); e != nil {
		apiError(w, 400, "invalid_input", "invalid JSON fields")
		return false
	}
	return true
}
