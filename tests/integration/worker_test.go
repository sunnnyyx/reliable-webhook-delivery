//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/httpapi"
	"relayforge/internal/netpolicy"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"relayforge/internal/worker"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentClaimersAndAtomicFencedCompletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Worker contention")
	targets(t, s, c.TenantID, "order.created")
	for i := 0; i < 20; i++ {
		if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
			t.Fatal(e)
		}
	}
	jobs := make(chan delivery.Job, 20)
	errs := make(chan error, 8)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				job, e := s.Claim(ctx, security.ID(), 30*time.Second)
				if e != nil {
					errs <- e
					return
				}
				if job == nil {
					return
				}
				jobs <- *job
			}
		}()
	}
	group.Wait()
	close(jobs)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	all := []delivery.Job{}
	seen := map[string]bool{}
	for j := range jobs {
		if seen[j.ID] || j.Attempt != 1 || j.LeaseToken == "" || len(j.Body) == 0 || len(j.Secrets) != 1 {
			t.Fatal("invalid or duplicate claim")
		}
		seen[j.ID] = true
		all = append(all, j)
	}
	if len(all) != 20 {
		t.Fatalf("claimed %d deliveries", len(all))
	}
	var attempts int
	if e := s.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_attempts WHERE outcome='started'").Scan(&attempts); e != nil || attempts != 20 {
		t.Fatal("attempt reservation not atomic")
	}
	job := all[0]
	wrong := job
	wrong.LeaseToken = security.ID()
	if ok, e := s.Complete(ctx, wrong, delivery.Classify(204)); e != nil || ok {
		t.Fatal("wrong token completed")
	}
	wrong = job
	wrong.AttemptID = security.ID()
	if ok, e := s.Complete(ctx, wrong, delivery.Classify(204)); e == nil || ok {
		t.Fatal("attempt/delivery update did not roll back")
	}
	state, e := s.GetDelivery(ctx, c.TenantID, job.ID)
	if e != nil || state.Status != "in_flight" {
		t.Fatal("partial completion survived rollback")
	}
	if ok, e := s.Complete(ctx, job, delivery.Classify(204)); e != nil || !ok {
		t.Fatal("valid completion rejected")
	}
	if ok, e := s.Complete(ctx, job, delivery.Classify(204)); e != nil || ok {
		t.Fatal("duplicate completion applied")
	}
	old := all[1]
	if _, e = s.DB.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", old.ID); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, old, delivery.Classify(204)); e != nil || ok {
		t.Fatal("expired lease completed")
	}
	// Explicit test-only SQL simulates recovery; automatic expiry recovery is milestone 5.
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "UPDATE delivery_attempts SET outcome='lease_expired',finished_at=clock_timestamp() WHERE id=$1", old.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "UPDATE deliveries SET status='retry_scheduled',lease_token=NULL,lease_expires_at=NULL,next_attempt_at=clock_timestamp() WHERE id=$1", old.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	fresh, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || fresh == nil || fresh.ID != old.ID || fresh.Attempt != 2 || fresh.LeaseToken == old.LeaseToken {
		t.Fatal("new lease not distinct")
	}
	if ok, e := s.Complete(ctx, old, delivery.Classify(204)); e != nil || ok {
		t.Fatal("stale worker overwrote new lease")
	}
	if ok, e := s.Complete(ctx, *fresh, delivery.Classify(204)); e != nil || !ok {
		t.Fatal("new lease completion failed")
	}
	history, next, e := s.ListAttempts(ctx, c.TenantID, old.ID, 1, nil)
	if e != nil || len(history) != 1 || history[0].Outcome != "lease_expired" || next == "" {
		t.Fatal("first attempt page invalid")
	}
	cursor, e := delivery.DecodeAttemptCursor(next, c.TenantID, old.ID)
	if e != nil {
		t.Fatal(e)
	}
	history, next, e = s.ListAttempts(ctx, c.TenantID, old.ID, 1, cursor)
	if e != nil || len(history) != 1 || history[0].Outcome != "success" || history[0].Number != 2 || next != "" {
		t.Fatal("second attempt page invalid")
	}
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	body := expect(t, a, "GET", "/v1/deliveries/"+old.ID+"/attempts", c.APIKey, "", 200)
	if bytes.Contains(body, []byte("lease_token")) || bytes.Contains(body, []byte("encrypted_secret")) {
		t.Fatal("attempt view exposed private fields")
	}
	other := credentials(t, s, "Other operator")
	expect(t, a, "GET", "/v1/deliveries/"+old.ID+"/attempts", other.APIKey, "", 404)
	_, e = s.DB.Exec(ctx, `INSERT INTO delivery_attempts(id,tenant_id,delivery_id,cycle_number,attempt_number,lease_token,worker_id,started_at,outcome) VALUES($1,$2,$3,99,1,$4,$5,clock_timestamp(),'started')`, security.ID(), other.TenantID, old.ID, security.ID(), security.ID())
	expectPG(t, e, "23503")

}
func TestSignedNetworkDispatchOccursOutsideDatabaseLocks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Healthy sender")
	other := credentials(t, s, "Other receiver tenant")
	var endpointID, deliveryID string
	var secret []byte
	var expectedBody []byte
	var callbackError error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Equal(raw, expectedBody) {
			callbackError = errors.New("outgoing bytes changed")
		}
		if signing.Verify(deliveryID, r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), raw, [][]byte{secret}, time.Now(), 5*time.Minute) != nil {
			callbackError = errors.New("signature verification failed")
		}
		tx, e := s.DB.Begin(r.Context())
		if e != nil {
			callbackError = e
			w.WriteHeader(500)
			return
		}
		defer tx.Rollback(r.Context())
		if _, e = tx.Exec(r.Context(), "SELECT id FROM endpoints WHERE id=$1 FOR UPDATE NOWAIT", endpointID); e != nil {
			callbackError = errors.New("endpoint lock held during HTTP")
		}
		if _, e = tx.Exec(r.Context(), "SELECT id FROM deliveries WHERE id=$1 FOR UPDATE NOWAIT", deliveryID); e != nil {
			callbackError = errors.New("delivery lock held during HTTP")
		}
		var started int
		if e = s.DB.QueryRow(r.Context(), "SELECT count(*) FROM delivery_attempts WHERE delivery_id=$1 AND outcome='started'", deliveryID).Scan(&started); e != nil || started != 1 {
			callbackError = errors.New("started attempt not committed before HTTP")
		}
		if callbackError != nil {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	devURL := "http://localhost:" + strings.Split(server.URL, ":")[2] + "/webhooks"
	policy, e := netpolicy.New("development", devURL, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	ep, encoded, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: devURL, Subscriptions: []string{"order.created"}})
	if e != nil {
		t.Fatal(e)
	}
	endpointID = ep.ID
	secret, e = signing.DecodeSecret(encoded)
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.Accept(ctx, c.TenantID, "healthy", []byte(orderBody))
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || job == nil {
		t.Fatal("no healthy claim")
	}
	deliveryID = job.ID
	expectedBody = job.Body
	client := worker.NewClient(policy, 3*time.Second, 1)
	defer client.CloseIdleConnections()
	sender := &worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
	result := sender.Send(ctx, *job)
	if result.Outcome != "success" || callbackError != nil {
		t.Fatalf("healthy dispatch failed: %s (%v)", result.ErrorCode, callbackError)
	}
	if ok, e := s.Complete(ctx, *job, result); e != nil || !ok {
		t.Fatal("healthy completion failed")
	}
	detail, e := s.GetEvent(ctx, c.TenantID, v.EventID)
	if e != nil || detail.DeliverySummary["delivered"] != 1 {
		t.Fatal("delivery not visible as acknowledged")
	}
	tampered := *job
	tampered.TenantID = other.TenantID
	if r := sender.Send(ctx, tampered); r.ErrorCode != "secret_unavailable" {
		t.Fatal("cross-tenant ciphertext reached network")
	}
}
func TestDisableDeleteAndClaimShareEndpointLockOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Lifecycle worker")
	ep := targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, "lifecycle", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM endpoints WHERE id=$1 FOR UPDATE", ep.ID); e != nil {
		t.Fatal(e)
	}
	job, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || job != nil {
		t.Fatal("claim did not skip locked endpoint")
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	no := false
	if _, e = s.UpdateEndpoint(ctx, c.TenantID, ep.ID, endpoints.Patch{Enabled: &no}); e != nil {
		t.Fatal(e)
	}
	if job, e = s.Claim(ctx, security.ID(), 30*time.Second); e != nil || job != nil {
		t.Fatal("disabled endpoint claimed")
	}
	var count int
	if e = s.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_attempts").Scan(&count); e != nil || count != 0 {
		t.Fatal("disabled deferral consumed attempt")
	}
	yes := true
	if _, e = s.UpdateEndpoint(ctx, c.TenantID, ep.ID, endpoints.Patch{Enabled: &yes}); e != nil {
		t.Fatal(e)
	}
	job, e = s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || job == nil {
		t.Fatal("enabled endpoint not claimed")
	}
	if e = s.DeleteEndpoint(ctx, c.TenantID, ep.ID); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(ctx, *job, delivery.Classify(204)); e != nil || ok {
		t.Fatal("completion overwrote deletion")
	}
	history, _, e := s.ListAttempts(ctx, c.TenantID, job.ID, 25, nil)
	if e != nil || len(history) != 1 || history[0].Outcome != "cancelled" || history[0].FinishedAt == nil {
		t.Fatal("deleted attempt history left started")
	}
	if job, e = s.Claim(ctx, security.ID(), 30*time.Second); e != nil || job != nil {
		t.Fatal("deleted endpoint claimed")
	}
	other := credentials(t, s, "Disabled tenant worker")
	targets(t, s, other.TenantID, "order.created")
	if _, e = s.Accept(ctx, other.TenantID, "disabled-tenant", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	if e = s.DisableTenant(ctx, other.TenantID); e != nil {
		t.Fatal(e)
	}
	if job, e = s.Claim(ctx, security.ID(), 30*time.Second); e != nil || job != nil {
		t.Fatal("disabled tenant claimed")
	}

}

func TestCompletionRechecksExpirationAfterLockWait(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := credentials(t, s, "Lease expiry lock wait")
	targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, "expiry-lock", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	job, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || job == nil {
		t.Fatal("no job")
	}
	if _, e = s.DB.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE id=$1", job.ID); e != nil {
		t.Fatal(e)
	}
	lock, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Rollback(ctx)
	if _, e = lock.Exec(ctx, "SELECT id FROM deliveries WHERE id=$1 FOR UPDATE", job.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan bool, 1)
	errs := make(chan error, 1)
	go func() { ok, e := s.Complete(ctx, *job, delivery.Classify(204)); done <- ok; errs <- e }()
	expired := false
	for !expired {
		if e = s.DB.QueryRow(ctx, "SELECT clock_timestamp()>lease_expires_at FROM deliveries WHERE id=$1", job.ID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if !expired {
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("expiry wait timed out")
			}
		}
	}
	if e = lock.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if ok, e := <-done, <-errs; e != nil || ok {
		t.Fatal("lease expired while waiting but completion applied")
	}
}

func TestWorkerFailureSchedulingKeepsBytesAndAttempts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Failure scheduling")
	targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, "schedule", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	first, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || first == nil {
		t.Fatal("no first claim")
	}
	if ok, e := s.Complete(ctx, *first, delivery.Classify(500)); e != nil || !ok {
		t.Fatal("retryable completion failed")
	}
	row, e := s.GetDelivery(ctx, c.TenantID, first.ID)
	if e != nil || row.Status != "retry_scheduled" || row.CycleAttemptCount != 1 {
		t.Fatal("retryable failure not durably scheduled")
	}
	if _, e = s.DB.Exec(ctx, "UPDATE deliveries SET next_attempt_at=clock_timestamp() WHERE id=$1", first.ID); e != nil {
		t.Fatal(e)
	}
	second, e := s.Claim(ctx, security.ID(), 30*time.Second)
	if e != nil || second == nil || second.Attempt != 2 || !bytes.Equal(second.Body, first.Body) || second.URL != first.URL {
		t.Fatal("retry changed bytes/destination or attempt identity")
	}
	if ok, e := s.Complete(ctx, *second, delivery.Classify(400)); e != nil || !ok {
		t.Fatal("permanent completion failed")
	}
	row, e = s.GetDelivery(ctx, c.TenantID, first.ID)
	if e != nil || row.Status != "dead_letter" || row.LifetimeAttemptCount != 2 {
		t.Fatal("permanent failure not terminal")
	}
	history, _, e := s.ListAttempts(ctx, c.TenantID, first.ID, 25, nil)
	if e != nil || len(history) != 2 || history[0].Outcome != "retryable_failure" || history[1].Outcome != "permanent_failure" {
		t.Fatal("failure history incorrect")
	}
}
