//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/httpapi"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"sync"
	"testing"
	"time"
)

func failedJob(t *testing.T, s *postgres.Store, tenant string) *delivery.Job {
	t.Helper()
	ctx := context.Background()
	if _, e := s.Accept(ctx, tenant, security.ID(), []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal("claim", e)
	}
	if ok, e := s.Complete(ctx, *j, delivery.Classify(400)); e != nil || !ok {
		t.Fatal("complete", e)
	}
	return j
}
func TestReplayConcurrencyIdentityAndHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Replay")
	targets(t, s, c.TenantID, "order.created")
	first := failedJob(t, s, c.TenantID)
	results := make(chan delivery.Replay, 20)
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, e := s.Replay(ctx, c.TenantID, first.ID, "same-key")
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var accepted delivery.Replay
	for r := range results {
		if accepted.ID == "" {
			accepted = r
		}
		if r != accepted || r.Cycle != 2 {
			t.Fatal("same key created multiple acceptances")
		}
	}
	if _, _, e := s.Replay(ctx, c.TenantID, first.ID, "different-key"); !errors.Is(e, delivery.ErrReplayState) {
		t.Fatal("replayed pending", e)
	}
	d, e := s.GetDelivery(ctx, c.TenantID, first.ID)
	if e != nil || d.CycleNumber != 2 || d.CycleAttemptCount != 0 || d.LifetimeAttemptCount != 1 || d.URLSnapshot != first.URL {
		t.Fatal("reset wrong fields", d, e)
	}
	second, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || second == nil {
		t.Fatal(e)
	}
	if second.ID != first.ID || second.EventID != first.EventID || second.URL != first.URL || !bytes.Equal(second.Body, first.Body) || second.Cycle != 2 || second.Attempt != 1 {
		t.Fatal("replay mutated identity/body")
	}
	if ok, e := s.Complete(ctx, *first, delivery.Classify(204)); e != nil || ok {
		t.Fatal("old cycle completion admitted", e)
	}
	if ok, e := s.Complete(ctx, *second, delivery.Classify(400)); e != nil || !ok {
		t.Fatal(e)
	}
	again, existing, e := s.Replay(ctx, c.TenantID, first.ID, "same-key")
	if e != nil || !existing || again != accepted {
		t.Fatal("old response lost after later failure", e)
	}
	third, _, e := s.Replay(ctx, c.TenantID, first.ID, "third-cycle")
	if e != nil || third.Cycle != 3 {
		t.Fatal(e)
	}
	// New destination applies only to new events. Replay keeps the original snapshot.
	changed := "https://hooks.example.com/new-destination"
	if _, e := s.UpdateEndpoint(ctx, c.TenantID, first.EndpointID, endpoints.Patch{URL: &changed}); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil || j.URL != first.URL {
		t.Fatal("snapshot changed", e)
	}
	if ok, e := s.Complete(ctx, *j, delivery.Classify(400)); e != nil || !ok {
		t.Fatal(e)
	}
	// Failed replay-history insert must roll back the cycle reset.
	s.NewID = func() string { return accepted.ID }
	if _, _, e := s.Replay(ctx, c.TenantID, first.ID, "force-insert-error"); e == nil {
		t.Fatal("expected duplicate replay ID failure")
	}
	s.NewID = nil
	d, e = s.GetDelivery(ctx, c.TenantID, first.ID)
	if e != nil || d.Status != "dead_letter" || d.CycleNumber != 3 || d.LifetimeAttemptCount != 3 {
		t.Fatal("replay partially committed", d, e)
	}
	history, _, e := s.ListAttempts(ctx, c.TenantID, first.ID, 100, nil)
	if e != nil || len(history) != 3 {
		t.Fatal(e)
	}
	for i, a := range history {
		if a.Cycle != i+1 || a.Number != 1 || a.Outcome != "permanent_failure" {
			t.Fatal("lost attempts")
		}
	}
	page, cursor, e := s.ListReplays(ctx, c.TenantID, first.ID, 1, nil)
	if e != nil || len(page) != 1 || cursor == "" || page[0].Cycle != 3 {
		t.Fatal("replay history missing", e)
	}
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	client := apiClient{server.URL, server.Client()}
	expect(t, client, "GET", "/v1/deliveries/"+first.ID+"/replays?limit=1&cursor="+cursor, c.APIKey, "", 200)
	// Composite tenant FK blocks cross-tenant replay audit rows.
	other := credentials(t, s, "Other")
	_, e = s.DB.Exec(ctx, `INSERT INTO delivery_replays VALUES($1,$2,$3,'cross-tenant',4,clock_timestamp())`, security.ID(), other.TenantID, first.ID)
	expectPG(t, e, "23503")
}

func TestReplayAPIValidationAndLostResponse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Replay API")
	other := credentials(t, s, "Other")
	targets(t, s, c.TenantID, "order.created")
	j := failedJob(t, s, c.TenantID)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	path := "/v1/deliveries/" + j.ID + "/replay"
	call := func(token, key, body string) (int, []byte, http.Header) {
		t.Helper()
		r, _ := http.NewRequest("POST", server.URL+path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(response.Body)
		if response.Header.Get("X-Request-ID") == "" {
			t.Fatal("request ID absent")
		}
		return response.StatusCode, b.Bytes(), response.Header
	}
	for _, tc := range []struct {
		token, key, body string
		status           int
	}{{c.APIKey, "", "{}", 400}, {c.APIKey, "k", "", 400}, {c.APIKey, "k", "null", 400}, {c.APIKey, "k", `{"url":"https://new.example.com"}`, 400}, {other.APIKey, "k", "{}", 404}, {"invalid", "k", "{}", 401}} {
		if code, _, _ := call(tc.token, tc.key, tc.body); code != tc.status {
			t.Fatalf("validation status %d want %d", code, tc.status)
		}
	}
	status, b, headers := call(c.APIKey, "accepted", "{}")
	if status != 202 || headers.Get("Idempotency-Replayed") != "false" {
		t.Fatal(status)
	}
	var replay delivery.Replay
	if e := json.Unmarshal(b, &replay); e != nil {
		t.Fatal(e)
	}
	// Complete work before retrying the previously lost API response.
	job, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || job == nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, *job, delivery.Classify(204)); e != nil || !ok {
		t.Fatal(e)
	}
	server.Close()
	server = httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	code, again, h := call(c.APIKey, "accepted", "{ }")
	if code != 202 || !bytes.Equal(b, again) || h.Get("Idempotency-Replayed") != "true" {
		t.Fatal("lost-response replay not stable")
	}
	if code, _, _ := call(c.APIKey, "new-key", "{}"); code != 409 {
		t.Fatal("delivered work replayed")
	}
	if e := s.DeleteEndpoint(ctx, c.TenantID, j.EndpointID); e != nil {
		t.Fatal(e)
	}
	if code, again, _ := call(c.APIKey, "accepted", "{}"); code != 202 || !bytes.Equal(b, again) {
		t.Fatal("idempotent acceptance lost after deletion")
	}

	client := apiClient{server.URL, server.Client()}
	expect(t, client, "GET", "/v1/deliveries/"+j.ID+"/replays", other.APIKey, "", 404)
	expect(t, client, "GET", "/v1/deliveries/"+j.ID+"/replays?limit=101", c.APIKey, "", 400)
	if events.ValidateKey("accepted") != nil {
		t.Fatal("test key invalid")
	}
}
func TestReplayStateRetentionAndAvailability(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Replay guards")
	targets(t, s, c.TenantID, "order.created")
	j := failedJob(t, s, c.TenantID)
	for _, state := range []string{"pending", "retry_scheduled", "delivered", "cancelled", "in_flight"} {
		_, e := s.DB.Exec(ctx, `UPDATE deliveries SET status=$1,lease_token=CASE WHEN $1='in_flight' THEN $2::uuid ELSE NULL END,lease_expires_at=CASE WHEN $1='in_flight' THEN clock_timestamp()+interval '1 minute' ELSE NULL END`, state, security.ID())
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = s.Replay(ctx, c.TenantID, j.ID, "guard"); !errors.Is(e, delivery.ErrReplayState) {
			t.Fatal(state, e)
		}
	}
	if _, e := s.DB.Exec(ctx, `UPDATE deliveries SET status='dead_letter',lease_token=NULL,lease_expires_at=NULL; UPDATE endpoints SET enabled=false`); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Replay(ctx, c.TenantID, j.ID, "guard"); !errors.Is(e, delivery.ErrReplayState) {
		t.Fatal("disabled endpoint", e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE endpoints SET enabled=true; UPDATE events SET expires_at=created_at+interval '1 microsecond'`); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Replay(ctx, c.TenantID, j.ID, "guard"); !errors.Is(e, delivery.ErrReplayState) {
		t.Fatal("expired event", e)
	}
	if e := s.DeleteEndpoint(ctx, c.TenantID, j.EndpointID); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Replay(ctx, c.TenantID, j.ID, "guard"); !errors.Is(e, delivery.ErrReplayState) {
		t.Fatal("deleted endpoint", e)
	}
}

func TestConcurrentDifferentReplayKeysStartOneCycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Distinct replay keys")
	targets(t, s, c.TenantID, "order.created")
	j := failedJob(t, s, c.TenantID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := s.Replay(ctx, c.TenantID, j.ID, security.ID())
			if e == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if !errors.Is(e, delivery.ErrReplayState) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatal("multiple cycles accepted", accepted)
	}
	history, _, e := s.ListReplays(ctx, c.TenantID, j.ID, 100, nil)
	if e != nil || len(history) != 1 {
		t.Fatal("multiple replay records", e)
	}
}

func TestReplayRechecksRetentionAfterEventLockWait(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Replay expiry lock")
	targets(t, s, c.TenantID, "order.created")
	j := failedJob(t, s, c.TenantID)
	locked, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer locked.Rollback(ctx)
	if _, e = locked.Exec(ctx, `UPDATE events SET expires_at=clock_timestamp()+interval '500 milliseconds' WHERE id=$1`, j.EventID); e != nil {
		t.Fatal(e)
	}
	// Commit expiry first, then hold the row so the replay must wait past it.
	if e = locked.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	locked, e = s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer locked.Rollback(ctx)
	if _, e = locked.Exec(ctx, `SELECT id FROM events WHERE id=$1 FOR UPDATE`, j.EventID); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() { _, _, e := s.Replay(ctx, c.TenantID, j.ID, "after-lock-wait"); result <- e }()
	deadline := time.Now().Add(3 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		var waiting bool
		e = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT expires_at FROM events%')`).Scan(&waiting)
		if e != nil {
			t.Fatal(e)
		}
		if waiting {
			observed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !observed {
		t.Fatal("replay never waited for event lock")
	}
	for time.Now().Before(deadline) {
		var expired bool
		if e = locked.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM events WHERE id=$1`, j.EventID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if e = locked.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-result:
		if !errors.Is(e, delivery.ErrReplayState) {
			t.Fatal("expired event replayed after lock wait", e)
		}
	case <-time.After(time.Second):
		t.Fatal("replay lock wait did not finish")
	}
}
