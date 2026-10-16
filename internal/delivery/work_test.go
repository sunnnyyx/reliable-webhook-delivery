package delivery

import (
	"net/http"
	"testing"
	"time"
)

func TestClassificationAndFiniteRetryScheduling(t *testing.T) {
	for _, status := range []int{200, 204, 299} {
		if Classify(status).Outcome != "success" {
			t.Fatal("2xx not acknowledged")
		}
	}
	for _, status := range []int{408, 425, 429, 500, 503, 599} {
		if Classify(status).Outcome != "retryable_failure" {
			t.Fatal("transient status not retryable")
		}
	}
	for _, status := range []int{301, 307, 400, 401, 404} {
		if Classify(status).Outcome != "permanent_failure" {
			t.Fatal("permanent status retried")
		}
	}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	job := Job{Attempt: 1, CycleStartedAt: now}
	r := Classify(503)
	r.RetryAfter = 30 * time.Second
	p := Next(job, r, now, func(int64) int64 { return 0 })
	if p.Status != "retry_scheduled" || !p.NextAt.Equal(now.Add(30*time.Second)) {
		t.Fatal("Retry-After not honored")
	}
	job.Attempt = 10
	if Next(job, r, now, nil).Status != "dead_letter" {
		t.Fatal("attempt budget not finite")
	}
	job.Attempt = 1
	job.CycleStartedAt = now.Add(-24*time.Hour + time.Second)
	if Next(job, r, now, nil).Status != "dead_letter" {
		t.Fatal("retry exceeded cycle deadline")
	}
	job.CycleStartedAt = now
	for attempt := 1; attempt <= 9; attempt++ {
		job.Attempt = attempt
		p = Next(job, Classify(500), now, func(n int64) int64 { return n - 1 })
		if p.NextAt.Sub(now) > time.Hour || p.NextAt.Before(now) {
			t.Fatal("invalid jitter bound")
		}
	}
	if RetryAfter("9999999999999", now) != time.Hour || RetryAfter("-1", now) != 0 || RetryAfter("bad", now) != 0 || RetryAfter(now.Add(time.Minute).Format(http.TimeFormat), now) != time.Minute {
		t.Fatal("Retry-After parsing failed")
	}
}
