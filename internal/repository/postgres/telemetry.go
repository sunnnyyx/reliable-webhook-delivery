package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/telemetry"
	"time"
)

func category(e error) string {
	if e == nil {
		return ""
	}
	var validation ValidationError
	switch {
	case errors.Is(e, ErrBacklog):
		return "backlog_limit"
	case errors.Is(e, events.ErrIdempotencyConflict):
		return "idempotency_mismatch"
	case errors.Is(e, delivery.ErrReplayState):
		return "invalid_state"
	case errors.Is(e, endpoints.ErrNotFound):
		return "not_found"
	case errors.As(e, &validation):
		return "invalid_input"
	default:
		return "storage_unavailable"
	}
}
func (s *Store) Accept(ctx context.Context, tenant, key string, raw []byte) (events.Accepted, error) {
	ctx, span := s.Telemetry.Start(ctx, "event.accept", attribute.String("tenant_id", tenant))
	result, e := s.accept(ctx, tenant, key, raw)
	if e == nil {
		span.SetAttributes(attribute.String("event_id", result.EventID))
		s.Telemetry.EntityLog(ctx, "event acceptance committed", "tenant_id", tenant, "event_id", result.EventID, "idempotent", result.Existing)
	}
	telemetry.Finish(span, category(e))
	return result, e
}
func (s *Store) Replay(ctx context.Context, tenant, id, key string) (delivery.Replay, bool, error) {
	ctx, span := s.Telemetry.Start(ctx, "delivery.replay", attribute.String("tenant_id", tenant), attribute.String("delivery_id", id))
	result, existing, e := s.replay(ctx, tenant, id, key)
	outcome := category(e)
	if e == nil {
		outcome = "accepted"
		if existing {
			outcome = "idempotent"
		}
		span.SetAttributes(attribute.Int("cycle", result.Cycle))
		s.Telemetry.EntityLog(ctx, "replay acceptance committed", "tenant_id", tenant, "delivery_id", id, "cycle", result.Cycle, "idempotent", existing)
	}
	s.Telemetry.Count(ctx, "replays", "result", outcome)
	telemetry.Finish(span, category(e))
	return result, existing, e
}
func (s *Store) Complete(ctx context.Context, job delivery.Job, result delivery.Result) (bool, error) {
	ctx, span := s.Telemetry.Start(ctx, "delivery.complete", attribute.String("delivery_id", job.ID), attribute.Int("cycle", job.Cycle), attribute.Int("attempt_number", job.Attempt))
	ok, e := s.complete(ctx, job, result)
	outcome := "committed"
	if e != nil {
		outcome = "storage_error"
	} else if !ok {
		outcome = "stale"
	}
	s.Telemetry.Count(ctx, "completions", "result", outcome)
	if ok && e == nil && result.Outcome == "success" && !job.EventCreatedAt.IsZero() {
		s.Telemetry.Duration(ctx, "event_to_ack_duration", time.Since(job.EventCreatedAt))
	}
	telemetry.Finish(span, category(e))
	return ok, e
}
func (s *Store) Observe(ctx context.Context) (telemetry.Snapshot, error) {
	result := telemetry.Snapshot{Counts: map[string]int64{}}
	rows, e := s.DB.Query(ctx, `SELECT d.status,count(*),COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-MIN(d.next_attempt_at) FILTER (WHERE d.status IN ('pending','retry_scheduled') AND d.next_attempt_at<=clock_timestamp() AND d.cycle_started_at>clock_timestamp()-interval '24 hours' AND d.cycle_attempt_count<10 AND e.enabled AND e.deleted_at IS NULL AND t.disabled_at IS NULL)),0)::double precision FROM deliveries d JOIN endpoints e ON e.tenant_id=d.tenant_id AND e.id=d.endpoint_id JOIN tenants t ON t.id=d.tenant_id GROUP BY d.status`)
	if e != nil {
		return result, e
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		var age float64
		if e = rows.Scan(&status, &count, &age); e != nil {
			return result, e
		}
		result.Counts[status] = count
		if age > result.OldestDueSeconds {
			result.OldestDueSeconds = age
		}
	}
	return result, rows.Err()
}
func (s *Store) saveTrace(ctx context.Context, tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, tenant, event string) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	_, e := tx.Exec(ctx, `INSERT INTO event_trace_context(tenant_id,event_id,trace_id,span_id,trace_flags) VALUES($1,$2,$3,$4,$5)`, tenant, event, sc.TraceID().String(), sc.SpanID().String(), int(sc.TraceFlags()))
	return e
}
