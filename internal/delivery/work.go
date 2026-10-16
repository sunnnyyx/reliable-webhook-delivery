package delivery

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

type Secret struct {
	ID            string
	Cipher, Nonce []byte
	Version       int
}
type Job struct {
	EventCreatedAt  time.Time
	TraceID, SpanID string
	TraceFlags      byte

	ID, TenantID, EndpointID, EventID, URL, LeaseToken, AttemptID, WorkerID string
	Body                                                                    []byte
	Secrets                                                                 []Secret
	Cycle, Attempt                                                          int
	CycleStartedAt, LeaseExpiresAt                                          time.Time
}
type Result struct {
	Outcome, ErrorCode string
	HTTPStatus         int
	LatencyMS          int64
	RetryAfter         time.Duration
}
type Plan struct {
	Status, ErrorCode string
	NextAt            time.Time
}

func Classify(status int) Result {
	r := Result{HTTPStatus: status}
	switch {
	case status >= 200 && status < 300:
		r.Outcome = "success"
	case status == 408 || status == 425 || status == 429 || status >= 500 && status <= 599:
		r.Outcome = "retryable_failure"
		r.ErrorCode = "http_retryable"
	case status >= 300 && status < 400:
		r.Outcome = "permanent_failure"
		r.ErrorCode = "redirect_blocked"
	default:
		r.Outcome = "permanent_failure"
		r.ErrorCode = "http_permanent"
	}
	return r
}
func RetryAfter(value string, now time.Time) time.Duration {
	if n, e := strconv.ParseInt(value, 10, 64); e == nil {
		if n < 0 {
			return 0
		}
		if n > 3600 {
			n = 3600
		}
		return time.Duration(n) * time.Second
	}
	if t, e := http.ParseTime(value); e == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		if d > time.Hour {
			return time.Hour
		}
		return d
	}
	return 0
}
func Next(job Job, result Result, now time.Time, random func(int64) int64) Plan {
	if result.Outcome == "success" {
		return Plan{Status: "delivered"}
	}
	p := Plan{Status: "dead_letter", ErrorCode: result.ErrorCode}
	if result.Outcome != "retryable_failure" {
		return p
	}
	if job.Attempt >= 10 {
		p.ErrorCode = "attempt_budget_exhausted"
		return p
	}
	deadline := job.CycleStartedAt.Add(24 * time.Hour)
	if !now.Before(deadline) {
		p.ErrorCode = "cycle_expired"
		return p
	}
	cap := 2 * time.Second
	for n := 1; n < job.Attempt && cap < time.Hour; n++ {
		cap *= 2
		if cap > time.Hour {
			cap = time.Hour
		}
	}
	if random == nil {
		random = rand.Int64N
	}
	delay := time.Duration(random(int64(cap) + 1))
	after := result.RetryAfter
	if after > time.Hour {
		after = time.Hour
	}
	if after > delay {
		delay = after
	}
	p.NextAt = now.Add(delay)
	if p.NextAt.After(deadline) {
		p.ErrorCode = "cycle_expired"
		return p
	}
	p.Status = "retry_scheduled"
	return p
}
