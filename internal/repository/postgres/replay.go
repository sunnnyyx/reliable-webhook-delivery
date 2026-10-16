package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/pagination"
	"time"
)

// Replay shares tenant -> endpoint -> delivery lock order with claim, recovery, and delete.
// A prior idempotent acceptance is returned before checking mutable availability.
func (s *Store) replay(ctx context.Context, tenant, id, key string) (delivery.Replay, bool, error) {
	var result delivery.Replay
	if e := events.ValidateKey(key); e != nil {
		return result, false, ValidationError{e}
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return result, false, e
	}
	defer tx.Rollback(ctx)
	if e = lockAdmission(ctx, tx, tenant); e != nil {
		return result, false, e
	}
	var endpoint string
	e = tx.QueryRow(ctx, `SELECT endpoint_id FROM deliveries WHERE tenant_id=$1 AND id=$2`, tenant, id).Scan(&endpoint)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	if e != nil {
		return result, false, e
	}
	var enabled bool
	var deleted *time.Time
	e = tx.QueryRow(ctx, `SELECT enabled,deleted_at FROM endpoints WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, endpoint).Scan(&enabled, &deleted)
	if e != nil {
		return result, false, e
	}
	var status, event string
	var cycle int
	e = tx.QueryRow(ctx, `SELECT status,event_id,cycle_number FROM deliveries WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id).Scan(&status, &event, &cycle)
	if e != nil {
		return result, false, e
	}
	e = tx.QueryRow(ctx, `SELECT id,delivery_id,new_cycle_number,created_at FROM delivery_replays WHERE tenant_id=$1 AND delivery_id=$2 AND idempotency_key=$3`, tenant, id, key).Scan(&result.ID, &result.DeliveryID, &result.Cycle, &result.CreatedAt)
	if e == nil {
		result.CreatedAt = result.CreatedAt.UTC()
		e = tx.Commit(ctx)
		return result, true, e
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return result, false, e
	}
	if status != "dead_letter" || !enabled || deleted != nil || cycle >= 2147483647 {
		return result, false, delivery.ErrReplayState
	}
	var expires, now time.Time
	e = tx.QueryRow(ctx, `SELECT expires_at FROM events WHERE tenant_id=$1 AND id=$2 FOR SHARE`, tenant, event).Scan(&expires)
	if e != nil {
		return result, false, e
	}
	// Read time only after the event lock is acquired, including a retention lock wait.
	if e = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); e != nil {
		return result, false, e
	}
	if !expires.After(now) {
		return result, false, delivery.ErrReplayState
	}
	if e = s.checkBacklog(ctx, tx, tenant, 1); e != nil {
		return result, false, e
	}
	result = delivery.Replay{ID: s.newID(), DeliveryID: id, Cycle: cycle + 1, CreatedAt: now.UTC()}
	_, e = tx.Exec(ctx, `UPDATE deliveries SET status='pending',cycle_number=cycle_number+1,cycle_attempt_count=0,cycle_started_at=$3,next_attempt_at=$3,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL,last_http_status=NULL,delivered_at=NULL,updated_at=$3 WHERE tenant_id=$1 AND id=$2`, tenant, id, now)
	if e != nil {
		return result, false, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO delivery_replays(id,tenant_id,delivery_id,idempotency_key,new_cycle_number,created_at) VALUES($1,$2,$3,$4,$5,$6)`, result.ID, tenant, id, key, result.Cycle, result.CreatedAt)
	if e != nil {
		return result, false, e
	}
	e = tx.Commit(ctx)
	return result, false, e
}
func (s *Store) ListReplays(ctx context.Context, tenant, id string, limit int, cursor *pagination.Cursor) ([]delivery.Replay, string, error) {
	if _, e := s.GetDelivery(ctx, tenant, id); e != nil {
		return nil, "", e
	}
	args := []any{tenant, id, limit + 1}
	query := `SELECT id,delivery_id,new_cycle_number,created_at FROM delivery_replays WHERE tenant_id=$1 AND delivery_id=$2`
	if cursor != nil {
		query += ` AND (created_at,id)<($4,$5)`
		args = append(args, cursor.Time, cursor.ID)
	}
	rows, e := s.DB.Query(ctx, query+` ORDER BY created_at DESC,id DESC LIMIT $3`, args...)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []delivery.Replay{}
	for rows.Next() {
		var r delivery.Replay
		if e = rows.Scan(&r.ID, &r.DeliveryID, &r.Cycle, &r.CreatedAt); e != nil {
			return nil, "", e
		}
		r.CreatedAt = r.CreatedAt.UTC()
		items = append(items, r)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = pagination.Encode(tenant, "replays", id, last.CreatedAt, last.ID)
	}
	return items, next, nil
}
