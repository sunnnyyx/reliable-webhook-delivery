package worker

import (
	"context"
	"io"
	"log/slog"
	"relayforge/internal/delivery"
	"sync"
	"testing"
	"time"
)

type fakeRepository struct {
	mu                  sync.Mutex
	claims, completions int
}

func (f *fakeRepository) Claim(context.Context, string, time.Duration) (*delivery.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	return &delivery.Job{}, nil
}
func (f *fakeRepository) Complete(context.Context, delivery.Job, delivery.Result) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completions++
	return true, nil
}

type dispatchFunc func(context.Context, delivery.Job) delivery.Result

func (f dispatchFunc) Send(c context.Context, j delivery.Job) delivery.Result { return f(c, j) }
func TestSlotsClaimOnlyImmediatelyAvailableCapacityAndDrain(t *testing.T) {
	repo := &fakeRepository{}
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{Repository: repo, ID: "worker", Concurrency: 3, Poll: time.Millisecond, Lease: time.Minute, RequestTimeout: time.Second, ShutdownTimeout: time.Second, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Sender: dispatchFunc(func(c context.Context, j delivery.Job) delivery.Result {
		entered <- struct{}{}
		select {
		case <-release:
		case <-c.Done():
		}
		return delivery.Classify(204)
	})}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker slots did not start")
		}
	}
	repo.mu.Lock()
	claims := repo.claims
	repo.mu.Unlock()
	if claims != 3 {
		t.Fatal("claims queued behind busy slots")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.claims != 3 || repo.completions != 3 {
		t.Fatal("shutdown claimed more work or lost active completions")
	}
}
func TestShutdownGraceCancelsBlockedHTTP(t *testing.T) {
	repo := &fakeRepository{}
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{Repository: repo, ID: "worker", Concurrency: 1, Poll: time.Millisecond, Lease: time.Minute, RequestTimeout: time.Hour, ShutdownTimeout: 20 * time.Millisecond, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Sender: dispatchFunc(func(c context.Context, j delivery.Job) delivery.Result {
		close(entered)
		<-c.Done()
		return delivery.Result{Outcome: "retryable_failure", ErrorCode: "request_cancelled"}
	})}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HTTP did not enter")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown grace did not cancel HTTP")
	}
}

type recoveryFunc func(context.Context, int) (int, error)

func (f recoveryFunc) Recover(ctx context.Context, n int) (int, error) { return f(ctx, n) }
func TestRecoveryLoopStartsImmediatelyAndStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan int, 1)
	r := Runner{Recovery: recoveryFunc(func(ctx context.Context, n int) (int, error) { called <- n; <-ctx.Done(); return 0, ctx.Err() }), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case n := <-called:
		if n != 100 {
			t.Fatal(n)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance prevented shutdown")
	}
}
