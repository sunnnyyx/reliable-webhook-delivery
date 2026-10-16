//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/httpapi"
	"relayforge/internal/netpolicy"
	"relayforge/internal/ratelimit"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"relayforge/internal/worker"
	"sync"
	"testing"
	"time"
)

func testLimiter(t *testing.T, prefix string, rate, burst int) *ratelimit.Redis {
	t.Helper()
	raw := os.Getenv("INTEGRATION_REDIS_URL")
	if raw == "" {
		t.Fatal("INTEGRATION_REDIS_URL required")
	}
	r, e := ratelimit.New(raw, prefix, rate, burst)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Ping(context.Background()); e != nil {
		r.Close()
		t.Fatal("local test Redis unavailable")
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func TestRedisBucketAtomicAcrossClientsAndRefill(t *testing.T) {
	ctx := context.Background()
	prefix := "rf-test-" + security.ID()
	first := testLimiter(t, prefix, 1, 3)
	second := testLimiter(t, prefix, 1, 3)
	tenant := security.ID()
	key := prefix + ":tenant:" + tenant
	t.Cleanup(func() { first.Client.Del(context.Background(), key) })
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	denied := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := first
			if i%2 == 1 {
				r = second
			}
			d, e := r.Allow(ctx, tenant)
			if e != nil {
				t.Error(e)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if d.Allowed {
				allowed++
			} else {
				denied++
				if d.Wait <= 0 || d.Wait > time.Second {
					t.Error("bad deferral delay", d.Wait)
				}
			}
		}(i)
	}
	wg.Wait()
	if allowed != 3 || denied != 21 {
		t.Fatal("shared burst exceeded", allowed, denied)
	}
	ttl, e := first.Client.PTTL(ctx, key).Result()
	if e != nil || ttl <= 0 || ttl > 6*time.Second {
		t.Fatal("bucket TTL invalid", ttl, e)
	}
	// Redis-clock eligibility, no worker-clock injection or forced token refill.
	deadline := time.Now().Add(2 * time.Second)
	refilled := false
	for time.Now().Before(deadline) {
		d, e := first.Allow(ctx, tenant)
		if e != nil {
			t.Fatal(e)
		}
		if d.Allowed {
			refilled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !refilled {
		t.Fatal("bucket did not refill")
	}
	mismatch := testLimiter(t, prefix, 2, 3)
	if _, e := mismatch.Allow(ctx, tenant); e == nil {
		t.Fatal("conflicting worker configuration admitted")
	}
	if e := first.Client.HSet(ctx, key, "tokens", "invalid").Err(); e != nil {
		t.Fatal(e)
	}
	if _, e := first.Allow(ctx, tenant); e == nil {
		t.Fatal("corrupt bucket silently granted a fresh burst")
	}

}
func TestLimiterDeferralPersistsWithoutAttemptBudget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Rate limited")
	targets(t, s, c.TenantID, "order.created")
	for i := 0; i < 3; i++ {
		if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
			t.Fatal(e)
		}
	}
	prefix := "rf-test-" + security.ID()
	limit := testLimiter(t, prefix, 1, 1)
	s.Limiter = limit
	t.Cleanup(func() { limit.Client.Del(context.Background(), prefix+":tenant:"+c.TenantID) })
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, *j, delivery.Classify(204)); e != nil || !ok {
		t.Fatal(e)
	}
	if j, e = s.Claim(ctx, security.ID(), time.Minute); e != nil || j != nil {
		t.Fatal("denied permission reserved work", e)
	}
	var attempts, live int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts`).Scan(&attempts); e != nil || attempts != 1 {
		t.Fatal(attempts, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE status='pending' AND cycle_attempt_count=0 AND lifetime_attempt_count=0`).Scan(&live); e != nil || live != 2 {
		t.Fatal(live, e)
	}
	// A fresh worker with the same prefix cannot bypass shared permission/cooldown.
	other := testLimiter(t, prefix, 1, 1)
	fresh := &postgres.Store{DB: s.DB, Vault: s.Vault, Limiter: other}
	if j, e = fresh.Claim(ctx, security.ID(), time.Minute); e != nil || j != nil {
		t.Fatal("fresh worker bypassed deferral", e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j, e = fresh.Claim(ctx, security.ID(), time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if j != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j == nil || j.Attempt != 1 {
		t.Fatal("deferred work did not resume without consumed attempts")
	}
}
func TestBacklogAdmissionConcurrentAndIdempotent(t *testing.T) {
	s := testStore(t)
	s.MaxTenantBacklog = 2
	ctx := context.Background()
	c := credentials(t, s, "Backlog")
	// One acceptance fills both slots; no partial fan-out may escape a rejection.
	targets(t, s, c.TenantID, "order.created")
	targets(t, s, c.TenantID, "order.created")
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := []events.Accepted{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody))
			if e == nil {
				mu.Lock()
				accepted = append(accepted, r)
				mu.Unlock()
			} else if !errors.Is(e, postgres.ErrBacklog) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if len(accepted) != 1 {
		t.Fatal("backlog overshot", len(accepted))
	}
	var n int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM ingestion_idempotency`).Scan(&n); e != nil || n != 1 {
		t.Fatal("rejected reservation persisted", n, e)
	}
	var key string
	if e := s.DB.QueryRow(ctx, `SELECT idempotency_key FROM ingestion_idempotency`).Scan(&key); e != nil {
		t.Fatal(e)
	}
	if r, e := s.Accept(ctx, c.TenantID, key, []byte(orderBody)); e != nil || !r.Existing || r.EventID != accepted[0].EventID {
		t.Fatal("full backlog broke idempotent retry", e)
	}
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	post := func(path, key string, want int) {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+path, bytes.NewBufferString("{}"))
		if path == "/v1/events" {
			req, _ = http.NewRequest("POST", server.URL+path, bytes.NewBufferString(orderBody))
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		response, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		if response.StatusCode != want || response.Header.Get("X-Request-ID") == "" {
			t.Fatal("bad backlog response", response.StatusCode)
		}
		if want == 429 && response.Header.Get("Retry-After") != "1" {
			t.Fatal("missing retry hint")
		}
	}
	post("/v1/events", "api-backlog-reject", 429)
	// Completing one delivery frees one slot, but a two-target fan-out still cannot fit.
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, *j, delivery.Classify(400)); e != nil || !ok {
		t.Fatal(e)
	}
	if _, e = s.Accept(ctx, c.TenantID, "still-too-large", []byte(orderBody)); !errors.Is(e, postgres.ErrBacklog) {
		t.Fatal("partial fan-out admitted", e)
	}
	if _, _, e = s.Replay(ctx, c.TenantID, j.ID, "fits-one-slot"); e != nil {
		t.Fatal("replay could not fill released capacity", e)
	}
	// Dead-letter one live delivery, then fill the freed slot with a single-target event.
	j, e = s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, *j, delivery.Classify(400)); e != nil || !ok {
		t.Fatal(e)
	}
	var endpoint string
	if e = s.DB.QueryRow(ctx, `SELECT id FROM endpoints WHERE id<>$1 LIMIT 1`, j.EndpointID).Scan(&endpoint); e != nil {
		t.Fatal(e)
	}
	disabled := false
	if _, e = s.UpdateEndpoint(ctx, c.TenantID, endpoint, endpoints.Patch{Enabled: &disabled}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Accept(ctx, c.TenantID, "fill-last-slot", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Replay(ctx, c.TenantID, j.ID, "backlog-reject"); !errors.Is(e, postgres.ErrBacklog) {
		t.Fatal("replay bypassed backlog", e)
	}
	post("/v1/deliveries/"+j.ID+"/replay", "api-replay-backlog", 429)
	if _, e = s.Accept(ctx, c.TenantID, "no-targets", []byte(`{"type":"unsubscribed.event","data":{}}`)); e != nil {
		t.Fatal("no-target event consumed delivery capacity", e)
	}

}

func TestTenantFairnessAcrossWorkerRunners(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	big := credentials(t, s, "Large queue")
	small := credentials(t, s, "Small queue")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"queue":"big"`)) {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	u, _ := url.Parse(receiver.URL)
	destination := "http://localhost:" + u.Port() + "/webhooks"
	policy, e := netpolicy.New("development", destination, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	for _, tenant := range []string{big.TenantID, small.TenantID} {
		if _, _, e = s.CreateEndpoint(ctx, tenant, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}}); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 100; i++ {
		if _, e = s.Accept(ctx, big.TenantID, security.ID(), []byte(`{"type":"order.created","data":{"queue":"big"}}`)); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		if _, e = s.Accept(ctx, small.TenantID, security.ID(), []byte(`{"type":"order.created","data":{"queue":"small"}}`)); e != nil {
			t.Fatal(e)
		}
	}
	runctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	client := worker.NewClient(policy, 10*time.Second, 4)
	defer client.CloseIdleConnections()
	for i := 0; i < 2; i++ {
		repository := &postgres.Store{DB: s.DB, Vault: s.Vault, Policy: policy, MaxTenantInFlight: 2}
		runner := &worker.Runner{Repository: repository, Sender: &worker.Sender{Client: client, Policy: policy, Vault: s.Vault}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ID: security.ID(), Concurrency: 2, Poll: 10 * time.Millisecond, Lease: 30 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: time.Second}
		wg.Add(1)
		go func() { defer wg.Done(); runner.Run(runctx) }()
	}
	defer func() { cancel(); releaseOnce.Do(func() { close(release) }); wg.Wait() }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("large tenant did not fill quota")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	smallDone := 0
	for time.Now().Before(deadline) {
		if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant_id=$1 AND status='delivered'`, small.TenantID).Scan(&smallDone); e != nil {
			t.Fatal(e)
		}
		if smallDone == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if smallDone != 2 {
		t.Fatal("large tenant starved small queue")
	}
	var inFlight, attempts int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant_id=$1 AND status='in_flight'`, big.TenantID).Scan(&inFlight); e != nil || inFlight != 2 {
		t.Fatal("shared tenant quota not enforced", inFlight, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE tenant_id=$1`, big.TenantID).Scan(&attempts); e != nil || attempts != 2 {
		t.Fatal("attempts created behind occupied quota", attempts, e)
	}
}

func TestLimiterOutageDefersAndAgeSweepStillRuns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Limiter outage")
	targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, "outage-event", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	// A closed ephemeral TCP listener is a deterministic unavailable dependency.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.Addr().String()
	listener.Close()
	limiter, e := ratelimit.New("redis://"+address+"/15", "rf-outage-"+security.ID(), 10, 20)
	if e != nil {
		t.Fatal(e)
	}
	defer limiter.Close()
	s.Limiter = limiter
	if job, e := s.Claim(ctx, security.ID(), time.Minute); e != nil || job != nil {
		t.Fatal("outage admitted dispatch", e)
	}
	var attempts, budget int
	var code string
	var delayed bool
	if e = s.DB.QueryRow(ctx, `SELECT cycle_attempt_count,last_error_code,next_attempt_at>clock_timestamp() FROM deliveries`).Scan(&budget, &code, &delayed); e != nil || budget != 0 || code != "limiter_unavailable" || !delayed {
		t.Fatal("outage not durably deferred", budget, code, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts`).Scan(&attempts); e != nil || attempts != 0 {
		t.Fatal("outage consumed attempts", attempts, e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE deliveries SET cycle_started_at=clock_timestamp()-interval '25 hours'`); e != nil {
		t.Fatal(e)
	}
	if n, e := s.Recover(ctx, 100); e != nil || n != 1 {
		t.Fatal("limiter outage blocked age recovery", n, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT last_error_code FROM deliveries`).Scan(&code); e != nil || code != "cycle_expired" {
		t.Fatal(code, e)
	}
}

func TestLifecycleMutationDuringAuthorizedHTTP(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "delete"}[deleted], func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			c := credentials(t, s, "Lifecycle race")
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(204)
			}))
			defer receiver.Close()
			defer once.Do(func() { close(release) })
			u, _ := url.Parse(receiver.URL)
			destination := "http://localhost:" + u.Port() + "/webhooks"
			policy, e := netpolicy.New("development", destination, nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			s.Policy = policy
			ep, _, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}})
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 2; i++ {
				if _, e = s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
					t.Fatal(e)
				}
			}
			job, e := s.Claim(ctx, security.ID(), time.Minute)
			if e != nil || job == nil {
				t.Fatal(e)
			}
			client := worker.NewClient(policy, 5*time.Second, 1)
			defer client.CloseIdleConnections()
			sender := worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
			result := make(chan delivery.Result, 1)
			go func() { result <- sender.Send(ctx, *job) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("HTTP gate not reached")
			}
			if deleted {
				e = s.DeleteEndpoint(ctx, c.TenantID, ep.ID)
			} else {
				disabled := false
				_, e = s.UpdateEndpoint(ctx, c.TenantID, ep.ID, endpoints.Patch{Enabled: &disabled})
			}
			if e != nil {
				t.Fatal(e)
			}
			if j, e := s.Claim(ctx, security.ID(), time.Minute); e != nil || j != nil {
				t.Fatal("mutation permitted new claim", e)
			}
			once.Do(func() { close(release) })
			var response delivery.Result
			select {
			case response = <-result:
			case <-time.After(time.Second):
				t.Fatal("authorized HTTP did not finish")
			}
			if response.HTTPStatus != 204 {
				t.Fatal(response.HTTPStatus)
			}
			ok, e := s.Complete(ctx, *job, response)
			if e != nil || ok == deleted {
				t.Fatal("lifecycle completion semantics wrong", ok, e)
			}
			history, _, e := s.ListAttempts(ctx, c.TenantID, job.ID, 100, nil)
			if e != nil || len(history) != 1 {
				t.Fatal(e)
			}
			want := "success"
			if deleted {
				want = "cancelled"
			}
			if history[0].Outcome != want {
				t.Fatal("attempt outcome", history[0].Outcome)
			}
			if !deleted {
				enabled := true
				if _, e = s.UpdateEndpoint(ctx, c.TenantID, ep.ID, endpoints.Patch{Enabled: &enabled}); e != nil {
					t.Fatal(e)
				}
				j, e := s.Claim(ctx, security.ID(), time.Minute)
				if e != nil || j == nil || j.Attempt != 1 {
					t.Fatal("enable did not resume without consumed attempts", e)
				}
			}
		})
	}
}

func TestTenantDisableSerializesAdmissionAndClaim(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := credentials(t, s, "Tenant disable race")
	targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, "before-disable", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `UPDATE tenants SET disabled_at=clock_timestamp() WHERE id=$1`, c.TenantID); e != nil {
		t.Fatal(e)
	}
	// The old MVCC view sees active, but the explicit tenant lock must prevent authorization.
	if job, e := s.Claim(ctx, security.ID(), time.Minute); e != nil || job != nil {
		t.Fatal("claim passed in-progress tenant disable", e)
	}
	accepted := make(chan error, 1)
	go func() { _, e := s.Accept(ctx, c.TenantID, "during-disable", []byte(orderBody)); accepted <- e }()
	waiting := false
	for ctx.Err() == nil {
		if e = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM tenants%FOR NO KEY UPDATE%')`).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("admission did not wait for tenant lifecycle lock")
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-accepted:
		if !errors.Is(e, endpoints.ErrNotFound) {
			t.Fatal("disabled tenant accepted new work", e)
		}
	case <-ctx.Done():
		t.Fatal("admission did not finish")
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); e != nil || n != 1 {
		t.Fatal("partial disabled admission", n, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM ingestion_idempotency`).Scan(&n); e != nil || n != 1 {
		t.Fatal("disabled reservation survived", n, e)
	}
}
