package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/pagination"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"strconv"
)

func registerEvents(routes *http.ServeMux, store *postgres.Store) {
	registerAttempts(routes, store)
	registerReplays(routes, store)
	routes.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			apiError(w, 400, "invalid_idempotency_key", "supply exactly one Idempotency-Key")
			return
		}
		if e := events.ValidateKey(keys[0]); e != nil {
			apiError(w, 400, "invalid_idempotency_key", e.Error())
			return
		}
		raw, ok := readJSON(w, r)
		if !ok {
			return
		}
		result, e := store.Accept(r.Context(), tenant(r), keys[0], raw)
		if e != nil {
			var validation postgres.ValidationError
			if errors.Is(e, postgres.ErrBacklog) || errors.Is(e, endpoints.ErrNotFound) || errors.Is(e, events.ErrIdempotencyConflict) || errors.As(e, &validation) {
				failure(w, e)
			} else {
				apiError(w, 503, "storage_unavailable", "durable storage is unavailable; retry with the same Idempotency-Key")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/events/"+result.EventID)
		w.Header().Set("Idempotency-Replayed", strconv.FormatBool(result.Existing))
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	})
	routes.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		limit, cursor, _, ok := listQuery(w, r, "events")
		if !ok {
			return
		}
		items, next, e := store.ListEvents(r.Context(), tenant(r), limit, cursor)
		if failure(w, e) {
			return
		}
		respond(w, 200, map[string]any{"data": items, "next_cursor": next})
	})
	routes.HandleFunc("GET /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, e := store.GetEvent(r.Context(), tenant(r), id)
		if failure(w, e) {
			return
		}
		respond(w, 200, v)
	})
	routes.HandleFunc("GET /v1/deliveries", func(w http.ResponseWriter, r *http.Request) {
		limit, cursor, f, ok := listQuery(w, r, "deliveries")
		if !ok {
			return
		}
		items, next, e := store.ListDeliveries(r.Context(), tenant(r), limit, cursor, f)
		if failure(w, e) {
			return
		}
		respond(w, 200, map[string]any{"data": items, "next_cursor": next})
	})
	routes.HandleFunc("GET /v1/deliveries/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, e := store.GetDelivery(r.Context(), tenant(r), id)
		if failure(w, e) {
			return
		}
		respond(w, 200, v)
	})
}
func listQuery(w http.ResponseWriter, r *http.Request, resource string) (int, *pagination.Cursor, delivery.Filters, bool) {
	bad := func() (int, *pagination.Cursor, delivery.Filters, bool) {
		apiError(w, 400, "invalid_query", "invalid pagination, cursor, or filters")
		return 0, nil, delivery.Filters{}, false
	}
	query, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return bad()
	}
	allowed := map[string]bool{"limit": true, "cursor": true}
	if resource == "deliveries" {
		allowed["status"] = true
		allowed["endpoint_id"] = true
		allowed["event_id"] = true
	}
	for k, v := range query {
		if !allowed[k] || len(v) != 1 {
			return bad()
		}
	}
	limit := 25
	if raw, ok := query["limit"]; ok {
		limit, e = strconv.Atoi(raw[0])
		if e != nil || limit < 1 || limit > 100 {
			return bad()
		}
	}
	f := delivery.Filters{Status: query.Get("status"), EndpointID: query.Get("endpoint_id"), EventID: query.Get("event_id")}
	for _, key := range []string{"status", "endpoint_id", "event_id"} {
		if values, ok := query[key]; ok && values[0] == "" {
			return bad()
		}
	}
	if f.Status != "" && !delivery.ValidStatus(f.Status) || f.EndpointID != "" && !security.ValidID(f.EndpointID) || f.EventID != "" && !security.ValidID(f.EventID) {
		return bad()
	}
	filter := ""
	if resource == "deliveries" {
		filter = f.Fingerprint()
	}
	var cursor *pagination.Cursor
	if raw, ok := query["cursor"]; ok {
		cursor, e = pagination.Decode(raw[0], tenant(r), resource, filter)
		if e != nil {
			return bad()
		}
	}
	return limit, cursor, f, true
}
