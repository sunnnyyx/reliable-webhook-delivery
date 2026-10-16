package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/pagination"
)

const eventColumns = "id,event_type,created_at,expires_at,body_sha256"

func scanEvent(row pgx.Row) (events.Event, error) {
	var v events.Event
	var hash []byte
	e := row.Scan(&v.ID, &v.Type, &v.CreatedAt, &v.ExpiresAt, &hash)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	v.CreatedAt = v.CreatedAt.UTC()
	v.ExpiresAt = v.ExpiresAt.UTC()
	v.BodySHA256 = hex.EncodeToString(hash)
	return v, e
}
func (s *Store) ListEvents(ctx context.Context, tenant string, limit int, cursor *pagination.Cursor) ([]events.Event, string, error) {
	args := []any{tenant, limit + 1}
	q := "SELECT " + eventColumns + " FROM events WHERE tenant_id=$1"
	if cursor != nil {
		q += " AND (created_at,id)<($3,$4)"
		args = append(args, cursor.Time, cursor.ID)
	}
	q += " ORDER BY created_at DESC,id DESC LIMIT $2"
	rows, e := s.DB.Query(ctx, q, args...)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []events.Event{}
	for rows.Next() {
		v, e := scanEvent(rows)
		if e != nil {
			return nil, "", e
		}
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		v := items[len(items)-1]
		next = pagination.Encode(tenant, "events", "", v.CreatedAt, v.ID)
	}
	return items, next, nil
}
func (s *Store) GetEvent(ctx context.Context, tenant, id string) (events.Detail, error) {
	var v events.Detail
	var hash []byte
	e := s.DB.QueryRow(ctx, "SELECT "+eventColumns+",outgoing_body FROM events WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&v.Event.ID, &v.Event.Type, &v.Event.CreatedAt, &v.Event.ExpiresAt, &hash, &v.Payload)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	if e != nil {
		return v, e
	}
	if e = s.DB.QueryRow(ctx, `SELECT COALESCE((SELECT trace_id FROM event_trace_context WHERE tenant_id=$1 AND event_id=$2),'')`, tenant, id).Scan(&v.TraceID); e != nil {
		return v, e
	}
	v.Event.CreatedAt = v.Event.CreatedAt.UTC()
	v.Event.ExpiresAt = v.Event.ExpiresAt.UTC()
	v.Event.BodySHA256 = hex.EncodeToString(hash)
	v.DeliverySummary = map[string]int{}
	rows, e := s.DB.Query(ctx, "SELECT status,count(*) FROM deliveries WHERE tenant_id=$1 AND event_id=$2 GROUP BY status", tenant, id)
	if e != nil {
		return v, e
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if e = rows.Scan(&status, &count); e != nil {
			return v, e
		}
		v.DeliverySummary[status] = count
	}
	return v, rows.Err()
}
