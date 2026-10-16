package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"math/rand/v2"
	"relayforge/internal/delivery"
	"relayforge/internal/security"
	"time"
)

// Claim locks scheduler -> tenant -> endpoint -> delivery. Redis admission is
// bounded to 100ms before reserving an attempt; webhook HTTP/decryption follow commit.
func (s *Store) Claim(ctx context.Context, worker string, lease time.Duration) (*delivery.Job, error) {
	if !security.ValidID(worker) || lease <= 0 || lease > 2*time.Minute {
		return nil, errors.New("invalid worker or lease")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var tenant, endpoint string
	quota := s.MaxTenantInFlight
	if quota <= 0 {
		quota = 4
	}
	// Stable statement-time due cutoffs permit index range bounds. Final cycle and
	// lease checks still use fresh database clock time after locks.
	// Scheduler-row locking serializes admission for one tenant across workers.
	// Least-recently-selected order and a shared in-flight quota bound monopolization.
	e = tx.QueryRow(ctx, `SELECT q.tenant_id FROM tenant_dispatch_state q JOIN tenants t ON t.id=q.tenant_id WHERE t.disabled_at IS NULL AND q.dispatch_after<=clock_timestamp() AND (SELECT count(*) FROM deliveries f WHERE f.tenant_id=q.tenant_id AND f.status='in_flight')<$1 AND EXISTS(SELECT 1 FROM endpoints e JOIN deliveries d ON d.tenant_id=e.tenant_id AND d.endpoint_id=e.id WHERE e.tenant_id=q.tenant_id AND e.enabled AND e.deleted_at IS NULL AND d.status IN ('pending','retry_scheduled') AND d.next_attempt_at<=statement_timestamp() AND d.cycle_attempt_count<10 AND d.cycle_started_at>clock_timestamp()-interval '24 hours') ORDER BY q.last_selected_at,q.tenant_id FOR UPDATE OF q SKIP LOCKED LIMIT 1`, quota).Scan(&tenant)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var active string
	e = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR SHARE SKIP LOCKED`, tenant).Scan(&active)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, yieldTenant(ctx, tx, tenant)
	}
	if e != nil {
		return nil, e
	}
	var flights int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant_id=$1 AND status='in_flight'`, tenant).Scan(&flights); e != nil {
		return nil, e
	}
	if flights >= quota {
		return nil, yieldTenant(ctx, tx, tenant)
	}
	e = tx.QueryRow(ctx, `SELECT e.id FROM endpoints e WHERE e.tenant_id=$1 AND e.enabled AND e.deleted_at IS NULL AND EXISTS(SELECT 1 FROM deliveries d WHERE d.tenant_id=e.tenant_id AND d.endpoint_id=e.id AND d.status IN ('pending','retry_scheduled') AND d.next_attempt_at<=statement_timestamp() AND d.cycle_attempt_count<10 AND d.cycle_started_at>clock_timestamp()-interval '24 hours') ORDER BY e.id FOR UPDATE OF e SKIP LOCKED LIMIT 1`, tenant).Scan(&endpoint)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, yieldTenant(ctx, tx, tenant)
	}
	if e != nil {
		return nil, e
	}
	var job delivery.Job
	job.TenantID = tenant
	job.EndpointID = endpoint
	job.WorkerID = worker
	e = tx.QueryRow(ctx, `SELECT id,event_id,url_snapshot,cycle_number,cycle_attempt_count,cycle_started_at FROM deliveries WHERE tenant_id=$1 AND endpoint_id=$2 AND status IN ('pending','retry_scheduled') AND next_attempt_at<=statement_timestamp() AND cycle_attempt_count<10 AND cycle_started_at>clock_timestamp()-interval '24 hours' ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, tenant, endpoint).Scan(&job.ID, &job.EventID, &job.URL, &job.Cycle, &job.Attempt, &job.CycleStartedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, yieldTenant(ctx, tx, tenant)
	}
	if e != nil {
		return nil, e
	}
	// A denial changes scheduling only: no lease, attempt row, or budget increment.
	if s.Limiter != nil {
		permission, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		decision, limitErr := s.Limiter.Allow(permission, tenant)
		cancel()
		if limitErr != nil || !decision.Allowed {
			delay := decision.Wait
			code := "rate_limited"
			if limitErr != nil {
				delay = time.Second
				code = "limiter_unavailable"
			}
			if delay < 25*time.Millisecond {
				delay = 25 * time.Millisecond
			}
			if delay > time.Hour {
				delay = time.Hour
			}
			delay += time.Duration(rand.Int64N(int64(50*time.Millisecond) + 1))
			if _, e = tx.Exec(ctx, `UPDATE tenant_dispatch_state SET dispatch_after=clock_timestamp()+($2::bigint*interval '1 microsecond'),last_selected_at=clock_timestamp() WHERE tenant_id=$1`, tenant, delay.Microseconds()); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE deliveries SET next_attempt_at=GREATEST(next_attempt_at,clock_timestamp()+($3::bigint*interval '1 microsecond')),last_error_code=$4,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, tenant, job.ID, delay.Microseconds(), code); e != nil {
				return nil, e
			}
			if e = tx.Commit(ctx); e != nil {
				return nil, e
			}
			s.Telemetry.Count(ctx, "rate_limit_deferrals", "reason", code)
			return nil, nil
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE tenant_dispatch_state SET last_selected_at=clock_timestamp() WHERE tenant_id=$1`, tenant); e != nil {
		return nil, e
	}
	var eligible bool
	if e = tx.QueryRow(ctx, `SELECT cycle_started_at>clock_timestamp()-interval '24 hours' AND cycle_attempt_count<10 FROM deliveries WHERE tenant_id=$1 AND id=$2`, tenant, job.ID).Scan(&eligible); e != nil {
		return nil, e
	}
	if !eligible {
		if _, e = tx.Exec(ctx, `UPDATE deliveries SET status='dead_letter',last_error_code='cycle_expired',updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, tenant, job.ID); e != nil {
			return nil, e
		}
		return nil, tx.Commit(ctx)
	}
	if e = tx.QueryRow(ctx, "SELECT outgoing_body,created_at FROM events WHERE tenant_id=$1 AND id=$2", tenant, job.EventID).Scan(&job.Body, &job.EventCreatedAt); e != nil {
		return nil, e
	}
	var flags int
	if e = tx.QueryRow(ctx, `SELECT COALESCE(c.trace_id,''),COALESCE(c.span_id,''),COALESCE(c.trace_flags,0) FROM events e LEFT JOIN event_trace_context c ON c.tenant_id=e.tenant_id AND c.event_id=e.id WHERE e.tenant_id=$1 AND e.id=$2`, tenant, job.EventID).Scan(&job.TraceID, &job.SpanID, &flags); e != nil {
		return nil, e
	}
	job.TraceFlags = byte(flags)
	rows, e := tx.Query(ctx, "SELECT id,encrypted_secret,nonce,encryption_key_version FROM endpoint_secrets WHERE tenant_id=$1 AND endpoint_id=$2 AND active_from<=clock_timestamp() AND (retire_after IS NULL OR retire_after>clock_timestamp()) ORDER BY active_from DESC,id", tenant, endpoint)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var v delivery.Secret
		if e = rows.Scan(&v.ID, &v.Cipher, &v.Nonce, &v.Version); e != nil {
			rows.Close()
			return nil, e
		}
		job.Secrets = append(job.Secrets, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	job.LeaseToken = s.newID()
	job.AttemptID = s.newID()
	job.Attempt++
	var started time.Time
	e = tx.QueryRow(ctx, `UPDATE deliveries SET status='in_flight',cycle_attempt_count=cycle_attempt_count+1,lifetime_attempt_count=lifetime_attempt_count+1,lease_token=$3,lease_expires_at=clock_timestamp()+($4::bigint*interval '1 microsecond'),updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 RETURNING lease_expires_at,updated_at`, tenant, job.ID, job.LeaseToken, lease.Microseconds()).Scan(&job.LeaseExpiresAt, &started)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO delivery_attempts(id,tenant_id,delivery_id,cycle_number,attempt_number,lease_token,worker_id,started_at,outcome) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'started')`, job.AttemptID, tenant, job.ID, job.Cycle, job.Attempt, job.LeaseToken, worker, started)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return &job, nil
}
func (s *Store) complete(ctx context.Context, job delivery.Job, result delivery.Result) (bool, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	// Acquire the row before evaluating expiration, including after a lock wait.
	var locked string
	e = tx.QueryRow(ctx, "SELECT id FROM deliveries WHERE tenant_id=$1 AND id=$2 AND status='in_flight' AND lease_token=$3 FOR UPDATE", job.TenantID, job.ID, job.LeaseToken).Scan(&locked)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	plan := delivery.Next(job, result, s.now(), nil)
	next := plan.NextAt
	if next.IsZero() {
		next = s.now()
	}
	tag, e := tx.Exec(ctx, `UPDATE deliveries SET status=$4,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULLIF($5,''),last_http_status=NULLIF($6,0),next_attempt_at=$7,delivered_at=CASE WHEN $4='delivered' THEN clock_timestamp() ELSE NULL END,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND status='in_flight' AND lease_token=$3 AND lease_expires_at>clock_timestamp()`, job.TenantID, job.ID, job.LeaseToken, plan.Status, plan.ErrorCode, result.HTTPStatus, next)
	if e != nil {
		return false, e
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	tag, e = tx.Exec(ctx, `UPDATE delivery_attempts SET outcome=$4,finished_at=clock_timestamp(),http_status=NULLIF($5,0),latency_ms=$6,error_code=NULLIF($7,'') WHERE tenant_id=$1 AND id=$2 AND delivery_id=$3 AND lease_token=$8 AND outcome='started'`, job.TenantID, job.AttemptID, job.ID, result.Outcome, result.HTTPStatus, result.LatencyMS, result.ErrorCode, job.LeaseToken)
	if e != nil {
		return false, e
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("attempt completion invariant failed")
	}
	if e = tx.Commit(ctx); e != nil {
		return false, e
	}
	return true, nil
}

// A locked tenant/endpoint or raced eligibility must yield to other tenants.
func yieldTenant(ctx context.Context, tx pgx.Tx, tenant string) error {
	if _, e := tx.Exec(ctx, `UPDATE tenant_dispatch_state SET last_selected_at=clock_timestamp() WHERE tenant_id=$1`, tenant); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
