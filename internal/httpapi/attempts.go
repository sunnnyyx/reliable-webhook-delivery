package httpapi

import (
	"net/http"
	"net/url"
	"relayforge/internal/delivery"
	"relayforge/internal/repository/postgres"
	"strconv"
)

func registerAttempts(routes *http.ServeMux, store *postgres.Store) {
	routes.HandleFunc("GET /v1/deliveries/{id}/attempts", func(w http.ResponseWriter, r *http.Request) {
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
				apiError(w, 400, "invalid_query", "invalid attempt pagination")
				return
			}
		}
		limit := 25
		if values, exists := q["limit"]; exists {
			limit, e = strconv.Atoi(values[0])
			if e != nil || limit < 1 || limit > 100 {
				apiError(w, 400, "invalid_query", "limit must be 1–100")
				return
			}
		}
		var cursor *delivery.AttemptCursor
		if values, exists := q["cursor"]; exists {
			cursor, e = delivery.DecodeAttemptCursor(values[0], tenant(r), id)
			if e != nil {
				apiError(w, 400, "invalid_cursor", "invalid attempt cursor")
				return
			}
		}
		items, next, e := store.ListAttempts(r.Context(), tenant(r), id, limit, cursor)
		if failure(w, e) {
			return
		}
		respond(w, 200, map[string]any{"data": items, "next_cursor": next})
	})
}
