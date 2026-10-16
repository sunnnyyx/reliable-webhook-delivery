package postgres

import (
	"context"
	"relayforge/internal/delivery"
)

func (s *Store) ListAttempts(ctx context.Context, tenant, id string, limit int, cursor *delivery.AttemptCursor) ([]delivery.Attempt, string, error) {
	if _, e := s.GetDelivery(ctx, tenant, id); e != nil {
		return nil, "", e
	}
	args := []any{tenant, id, limit + 1}
	query := "SELECT id,cycle_number,attempt_number,worker_id,started_at,finished_at,outcome,http_status,latency_ms,error_code FROM delivery_attempts WHERE tenant_id=$1 AND delivery_id=$2"
	if cursor != nil {
		query += " AND (cycle_number,attempt_number)>($4,$5)"
		args = append(args, cursor.Cycle, cursor.Attempt)
	}
	query += " ORDER BY cycle_number,attempt_number LIMIT $3"
	rows, e := s.DB.Query(ctx, query, args...)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []delivery.Attempt{}
	for rows.Next() {
		var a delivery.Attempt
		if e = rows.Scan(&a.ID, &a.Cycle, &a.Number, &a.WorkerID, &a.StartedAt, &a.FinishedAt, &a.Outcome, &a.HTTPStatus, &a.LatencyMS, &a.ErrorCode); e != nil {
			return nil, "", e
		}
		a.StartedAt = a.StartedAt.UTC()
		if a.FinishedAt != nil {
			a.FinishedAt = ptrTimeUTC(*a.FinishedAt)
		}
		items = append(items, a)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = delivery.EncodeAttemptCursor(tenant, id, items[len(items)-1])
	}
	return items, next, nil
}
