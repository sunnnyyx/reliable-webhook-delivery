package worker

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"math/rand/v2"
	"relayforge/internal/delivery"
	"relayforge/internal/telemetry"
	"sync"
	"time"
)

type Repository interface {
	Claim(context.Context, string, time.Duration) (*delivery.Job, error)
	Complete(context.Context, delivery.Job, delivery.Result) (bool, error)
}
type Dispatcher interface {
	Send(context.Context, delivery.Job) delivery.Result
}
type Recovery interface {
	Recover(context.Context, int) (int, error)
}
type Runner struct {
	Telemetry *telemetry.Runtime

	Recovery         Recovery
	RecoveryInterval time.Duration

	Repository                                   Repository
	Sender                                       Dispatcher
	Log                                          *slog.Logger
	ID                                           string
	Concurrency                                  int
	Poll, Lease, RequestTimeout, ShutdownTimeout time.Duration
}

func (r *Runner) Run(ctx context.Context) {
	active, cancel := context.WithCancel(context.Background())
	defer cancel()
	var group sync.WaitGroup
	if r.Recovery != nil {
		group.Add(1)
		go func() { defer group.Done(); r.recover(ctx) }()
	}
	for i := 0; i < r.Concurrency; i++ {
		group.Add(1)
		go func() { defer group.Done(); r.slot(ctx, active) }()
	}
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	timer := time.NewTimer(r.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		cancel()
		<-done
	}
}
func (r *Runner) slot(claims, active context.Context) {
	for {
		if claims.Err() != nil {
			return
		}
		work, cancelWork := context.WithTimeout(active, r.RequestTimeout)
		claim, cancelClaim := context.WithTimeout(claims, 5*time.Second)
		job, e := r.Repository.Claim(claim, r.ID, r.Lease)
		cancelClaim()
		if e != nil || job == nil {
			cancelWork()
			if e != nil && claims.Err() == nil {
				r.Log.Error("claim failed", "error_code", "storage_unavailable", "worker_id", r.ID)
			}
			delay := r.Poll*3/4 + time.Duration(rand.Int64N(int64(r.Poll/2)+1))
			timer := time.NewTimer(delay)
			select {
			case <-claims.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		work, span := r.Telemetry.Dispatch(work, job.TraceID, job.SpanID, job.TraceFlags, attribute.String("tenant_id", job.TenantID), attribute.String("event_id", job.EventID), attribute.String("delivery_id", job.ID), attribute.String("worker_id", r.ID), attribute.Int("cycle", job.Cycle), attribute.Int("attempt_number", job.Attempt))
		if job.Cycle == 1 && job.Attempt == 1 && !job.EventCreatedAt.IsZero() {
			r.Telemetry.Duration(work, "first_attempt_queue_delay", time.Since(job.EventCreatedAt))
		}
		dispatchStart := time.Now()
		result := r.Sender.Send(work, *job)
		r.Telemetry.DispatchResult(work, result.Outcome, result.HTTPStatus, time.Since(dispatchStart))
		cancelWork()
		complete, cancelComplete := context.WithTimeout(context.WithoutCancel(work), 5*time.Second)
		ok, e := r.Repository.Complete(complete, *job, result)
		cancelComplete()
		message := "attempt completed"
		if e != nil {
			message = "completion persistence failed"
		} else if !ok {
			message = "stale completion rejected"
		}
		if r.Telemetry == nil {
			r.Log.Info(message, "worker_id", r.ID, "tenant_id", job.TenantID, "event_id", job.EventID, "delivery_id", job.ID, "cycle", job.Cycle, "attempt_number", job.Attempt, "outcome", result.Outcome, "error_code", result.ErrorCode, "http_status", result.HTTPStatus, "latency_ms", result.LatencyMS)
		}
		span.SetAttributes(attribute.String("outcome", result.Outcome), attribute.Int("http.status_code", result.HTTPStatus))
		if r.Telemetry != nil {
			r.Telemetry.EntityLog(work, message, "worker_id", r.ID, "tenant_id", job.TenantID, "event_id", job.EventID, "delivery_id", job.ID, "cycle", job.Cycle, "attempt_number", job.Attempt, "outcome", result.Outcome, "error_code", result.ErrorCode, "http_status", result.HTTPStatus, "latency_ms", result.LatencyMS)
		}
		category := result.ErrorCode
		completionResult := "committed"
		if e != nil {
			category = "storage_unavailable"
			completionResult = "storage_error"
		} else if !ok {
			category = "stale_completion"
			completionResult = "stale"
		}
		span.SetAttributes(attribute.String("completion_result", completionResult))
		telemetry.Finish(span, category)
	}
}

// Maintenance stops with claims; in-progress dispatch still receives its drain period.
func (r *Runner) recover(ctx context.Context) {
	interval := r.RecoveryInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		count, err := r.Recovery.Recover(bounded, 100)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.Log.Error("recovery failed", "error_code", "storage_unavailable", "worker_id", r.ID)
		} else if count > 0 {
			r.Log.Info("recovery completed", "transitions", count, "worker_id", r.ID)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
