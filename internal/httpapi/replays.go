package httpapi

import (
	"net/http"
	"net/url"
	"relayforge/internal/events"
	"relayforge/internal/pagination"
	"relayforge/internal/repository/postgres"
	"strconv"
)

func registerReplays(routes *http.ServeMux, store *postgres.Store) {
	routes.HandleFunc("POST /v1/deliveries/{id}/replay", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || events.ValidateKey(keys[0]) != nil {
			apiError(w, 400, "invalid_idempotency_key", "supply one Idempotency-Key with 1–128 printable ASCII characters")
			return
		}
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
		result, existing, e := store.Replay(r.Context(), tenant(r), id, keys[0])
		if failure(w, e) {
			return
		}
		w.Header().Set("Idempotency-Replayed", strconv.FormatBool(existing))
		w.Header().Set("Location", "/v1/deliveries/"+id+"/replays")
		respond(w, 202, result)
	})
	routes.HandleFunc("GET /v1/deliveries/{id}/replays", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil {
			apiError(w, 400, "invalid_query", "malformed query")
			return
		}
		for k, v := range q {
			if (k != "limit" && k != "cursor") || len(v) != 1 {
				apiError(w, 400, "invalid_query", "invalid replay pagination")
				return
			}
		}
		limit := 25
		if v, ok := q["limit"]; ok {
			limit, e = strconv.Atoi(v[0])
			if e != nil || limit < 1 || limit > 100 {
				apiError(w, 400, "invalid_query", "limit must be 1–100")
				return
			}
		}
		var cursor *pagination.Cursor
		if v, ok := q["cursor"]; ok {
			cursor, e = pagination.Decode(v[0], tenant(r), "replays", id)
			if e != nil {
				apiError(w, 400, "invalid_cursor", "invalid replay cursor")
				return
			}
		}
		items, next, e := store.ListReplays(r.Context(), tenant(r), id, limit, cursor)
		if failure(w, e) {
			return
		}
		respond(w, 200, map[string]any{"data": items, "next_cursor": next})
	})
}
