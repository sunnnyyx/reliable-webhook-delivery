package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"relayforge/internal/delivery"
	"time"
)

// Recover performs bounded maintenance, including work on disabled endpoints.
// Each transition commits separately; endpoint -> delivery -> attempt matches Claim/Delete.
func (s *Store) Recover(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid recovery batch")
	}
	count := 0
	// Separate passes prevent a stream of expired leases starving aged queued work.
	for _, expired := range []bool{true, false} {
		for n := 0; n < limit; n++ {
			changed, err := s.recoverOne(ctx, expired)
			if err != nil {
				return count, err
			}
			if !changed {
				break
			}
			count++
		}
	}
	return count, nil
}
func (s *Store) recoverOne(ctx context.Context, expired bool) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	predicate := `d.status IN ('pending','retry_scheduled') AND (d.cycle_started_at<=clock_timestamp()-interval '24 hours' OR d.cycle_attempt_count>=10)`
	if expired {
		predicate = `d.status='in_flight' AND d.lease_expires_at<=clock_timestamp()`
	}
	var tenant, endpoint string
	err = tx.QueryRow(ctx, `SELECT e.tenant_id,e.id FROM endpoints e WHERE EXISTS(SELECT 1 FROM deliveries d WHERE d.tenant_id=e.tenant_id AND d.endpoint_id=e.id AND `+predicate+`) ORDER BY e.id FOR UPDATE OF e SKIP LOCKED LIMIT 1`).Scan(&tenant, &endpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var job delivery.Job
	job.TenantID = tenant
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT d.id,d.cycle_number,d.cycle_attempt_count,d.cycle_started_at,COALESCE(d.lease_token::text,''),clock_timestamp() FROM deliveries d WHERE d.tenant_id=$1 AND d.endpoint_id=$2 AND `+predicate+` ORDER BY d.cycle_started_at,d.id FOR UPDATE SKIP LOCKED LIMIT 1`, tenant, endpoint).Scan(&job.ID, &job.Cycle, &job.Attempt, &job.CycleStartedAt, &job.LeaseToken, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	plan := delivery.Plan{Status: "dead_letter", ErrorCode: "cycle_expired"}
	if job.Attempt >= 10 {
		plan.ErrorCode = "attempt_budget_exhausted"
	}
	if expired {
		plan = delivery.Next(job, delivery.Result{Outcome: "retryable_failure", ErrorCode: "lease_expired"}, now, nil)
		tag, e := tx.Exec(ctx, `UPDATE delivery_attempts SET outcome='lease_expired',finished_at=clock_timestamp(),error_code='lease_expired' WHERE tenant_id=$1 AND delivery_id=$2 AND lease_token=$3 AND outcome='started'`, tenant, job.ID, job.LeaseToken)
		if e != nil {
			return false, e
		}
		if tag.RowsAffected() != 1 {
			return false, errors.New("recovery attempt invariant failed")
		}
	}
	next := plan.NextAt
	if next.IsZero() {
		next = now
	}
	_, err = tx.Exec(ctx, `UPDATE deliveries SET status=$3,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=$4,last_error_code=$5,last_http_status=NULL,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, tenant, job.ID, plan.Status, next, plan.ErrorCode)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	reason := plan.ErrorCode
	if expired {
		reason = "lease_expired"
	}
	s.Telemetry.Count(ctx, "recoveries", "reason", reason)
	traceCtx, span := s.Telemetry.Start(ctx, "delivery.recover", attribute.String("delivery_id", job.ID), attribute.String("tenant_id", tenant), attribute.String("reason", reason))
	s.Telemetry.EntityLog(traceCtx, "delivery recovered", "tenant_id", tenant, "delivery_id", job.ID, "cycle", job.Cycle, "outcome", plan.Status, "error_code", plan.ErrorCode)
	span.End()
	return true, nil
}
