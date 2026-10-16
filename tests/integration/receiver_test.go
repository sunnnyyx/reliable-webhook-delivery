//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/stdlib"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/netpolicy"
	"relayforge/internal/receiver"
	"relayforge/internal/receiver/schema"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"relayforge/internal/worker"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func receiverSchema(t *testing.T, s *postgres.Store) {
	t.Helper()
	db := stdlib.OpenDB(*s.DB.Config().ConnConfig)
	defer db.Close()
	p, e := schema.Provider(db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func signedPost(client *http.Client, url, id string, raw []byte, keys [][]byte) (int, error) {
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	sig, e := signing.Sign(id, stamp, raw, keys)
	if e != nil {
		return 0, e
	}
	req, e := http.NewRequest("POST", url, bytes.NewReader(raw))
	if e != nil {
		return 0, e
	}
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", stamp)
	req.Header.Set("webhook-signature", sig)
	response, e := client.Do(req)
	if e != nil {
		return 0, e
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}
func receiverLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func TestReceiverTransactionalDeduplicationAndNamespaces(t *testing.T) {
	s := testStore(t)
	receiverSchema(t, s)
	ctx := context.Background()
	key := bytes.Repeat([]byte{9}, 32)
	event := events.Envelope(security.ID(), time.Now(), events.Input{Type: "order.created", Data: json.RawMessage(`{"n":1}`)})
	processor := &receiver.Postgres{DB: s.DB, Source: "tenant-a-source"}
	server := httptest.NewServer(receiver.Handler([][]byte{key}, receiverLog(), nil, processor))
	defer server.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), event, [][]byte{key})
			if e != nil || code != 204 {
				t.Errorf("duplicate status %d err=%v", code, e)
			}
		}()
	}
	wg.Wait()
	var n int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM receiver_business_events`).Scan(&n); e != nil || n != 1 {
		t.Fatal("business effect count", n, e)
	}
	// Marker survives a new handler/processor instance and keys are checked even for duplicates.
	server.Close()
	server = httptest.NewServer(receiver.Handler([][]byte{key}, receiverLog(), nil, &receiver.Postgres{DB: s.DB, Source: processor.Source}))
	defer server.Close()
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), event, [][]byte{key}); e != nil || code != 204 {
		t.Fatal(code, e)
	}
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), event, [][]byte{bytes.Repeat([]byte{8}, 32)}); e != nil || code != 401 {
		t.Fatal("duplicate bypassed signature", code, e)
	}
	changed := append(append([]byte{}, event...), byte(' '))
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), changed, [][]byte{key}); e != nil || code != 409 {
		t.Fatal("event ID reused with changed bytes", code, e)
	}
	other := httptest.NewServer(receiver.Handler([][]byte{key}, receiverLog(), nil, &receiver.Postgres{DB: s.DB, Source: "tenant-b-source"}))
	defer other.Close()
	if code, e := signedPost(other.Client(), other.URL+"/webhooks", security.ID(), event, [][]byte{key}); e != nil || code != 204 {
		t.Fatal(code, e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM receiver_business_events`).Scan(&n); e != nil || n != 2 {
		t.Fatal("namespaces collided", n, e)
	}
	// Inject failure at the business write; a committed marker must not survive it.
	_, e := s.DB.Exec(ctx, `CREATE FUNCTION fail_demo_effect() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test failure'; END $$; CREATE TRIGGER fail_demo_effect BEFORE INSERT ON receiver_business_events FOR EACH ROW EXECUTE FUNCTION fail_demo_effect()`)
	if e != nil {
		t.Fatal(e)
	}
	fresh := events.Envelope(security.ID(), time.Now(), events.Input{Type: "order.created", Data: json.RawMessage(`{}`)})
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), fresh, [][]byte{key}); e != nil || code != 503 {
		t.Fatal("write failure acknowledged", code, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM receiver_processed_events`).Scan(&n); e != nil || n != 2 {
		t.Fatal("marker committed without effect", n, e)
	}
	if _, e = s.DB.Exec(ctx, `DROP TRIGGER fail_demo_effect ON receiver_business_events; DROP FUNCTION fail_demo_effect()`); e != nil {
		t.Fatal(e)
	}
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), fresh, [][]byte{key}); e != nil || code != 204 {
		t.Fatal("failed transaction not retryable", code, e)
	}
	// A receiver storage outage must fail closed, including already-seen events.
	s.DB.Close()
	ready, e := server.Client().Get(server.URL + "/health/ready")
	if e != nil {
		t.Fatal(e)
	}
	ready.Body.Close()
	if ready.StatusCode != 503 {
		t.Fatal("closed storage reported ready")
	}
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", security.ID(), event, [][]byte{key}); e != nil || code != 503 {
		t.Fatal("outage acknowledged duplicate", code, e)
	}

}
func TestReceiverMigrationRoundTrip(t *testing.T) {
	s := testStore(t)
	db := stdlib.OpenDB(*s.DB.Config().ConnConfig)
	defer db.Close()
	ctx := context.Background()
	p, e := schema.Provider(db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = p.DownTo(ctx, 0); e != nil {
		t.Fatal(e)
	}
	var sender, own bool
	if e = db.QueryRowContext(ctx, `SELECT to_regclass('deliveries') IS NOT NULL,to_regclass('receiver_processed_events') IS NOT NULL`).Scan(&sender, &own); e != nil || !sender || own {
		t.Fatal("receiver migration affected sender", e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestRotationQueuedDeliveriesAndRetiredKeys(t *testing.T) {
	s := testStore(t)
	receiverSchema(t, s)
	ctx := context.Background()
	c := credentials(t, s, "Rotation")
	processor := &receiver.Postgres{DB: s.DB, Source: "rotation-source"}
	var handler atomic.Value
	handler.Store(receiver.Handler([][]byte{bytes.Repeat([]byte{1}, 32)}, receiverLog(), nil, processor))
	var signatures atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			signatures.Store(int32(len(bytes.Fields([]byte(r.Header.Get("webhook-signature"))))))
		}
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	destination := "http://localhost:" + u.Port() + "/webhooks"
	policy, e := netpolicy.New("development", destination, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	ep, oldSecret, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}})
	if e != nil {
		t.Fatal(e)
	}
	oldKey, _ := signing.DecodeSecret(oldSecret)
	setKeys := func(keys [][]byte) { handler.Store(receiver.Handler(keys, receiverLog(), nil, processor)) }
	client := worker.NewClient(policy, time.Second, 1)
	defer client.CloseIdleConnections()
	sender := worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
	queued := func() {
		t.Helper()
		if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
			t.Fatal(e)
		}
	}
	dispatch := func(want, count int) *delivery.Job {
		t.Helper()
		j, e := s.Claim(ctx, security.ID(), time.Minute)
		if e != nil || j == nil {
			t.Fatal(e)
		}
		r := sender.Send(ctx, *j)
		if r.HTTPStatus != want || int(signatures.Load()) != count {
			t.Fatalf("rotation HTTP %d/signatures %d want %d/%d", r.HTTPStatus, signatures.Load(), want, count)
		}
		if ok, e := s.Complete(ctx, *j, r); e != nil || !ok {
			t.Fatal(e)
		}
		return j
	}
	setKeys([][]byte{oldKey})
	queued()
	dispatch(204, 1)
	// Queue before rotating: authorization must load current keys when Claim runs.
	queued()
	newSecret, e := s.RotateSecret(ctx, c.TenantID, ep.ID)
	if e != nil {
		t.Fatal(e)
	}
	newKey, _ := signing.DecodeSecret(newSecret)
	dispatch(204, 2) // receiver has only old key during overlap
	setKeys([][]byte{oldKey, newKey})
	queued()
	dispatch(204, 2)
	setKeys([][]byte{newKey})
	queued()
	dispatch(204, 2)
	// Advance only the isolated test's retirement boundary, not production configuration.
	if _, e = s.DB.Exec(ctx, `UPDATE endpoint_secrets SET active_from=clock_timestamp()-interval '25 hours',retire_after=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND endpoint_id=$2 AND retire_after IS NOT NULL`, c.TenantID, ep.ID); e != nil {
		t.Fatal(e)
	}
	queued()
	dispatch(204, 1)
	setKeys([][]byte{oldKey})
	queued()
	j := dispatch(401, 1)
	// Migrating the receiver to only new key permits replay with the same identity/body.
	setKeys([][]byte{newKey})
	if _, _, e = s.Replay(ctx, c.TenantID, j.ID, "after-retirement"); e != nil {
		t.Fatal(e)
	}
	retried := dispatch(204, 1)
	if !bytes.Equal(j.Body, retried.Body) || j.ID != retried.ID {
		t.Fatal("rotation replay changed event")
	}
	// An old-only signed request is rejected by the new-only receiver after retirement.
	if code, e := signedPost(server.Client(), server.URL+"/webhooks", j.ID, j.Body, [][]byte{oldKey}); e != nil || code != 401 {
		t.Fatal("retired key accepted", code, e)
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM receiver_business_events`).Scan(&n); e != nil || n != 6 {
		t.Fatal("rotation effects", n, e)
	}
}

func TestKilledWorkerAcknowledgmentDeduplicated(t *testing.T) {
	s := testStore(t)
	receiverSchema(t, s)
	ctx := context.Background()
	c := credentials(t, s, "Crash deduplication")
	processor := &receiver.Postgres{DB: s.DB, Source: "crash-source"}
	var handler atomic.Value
	handler.Store(receiver.Handler([][]byte{bytes.Repeat([]byte{1}, 32)}, receiverLog(), nil, processor))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	destination := "http://localhost:" + u.Port() + "/webhooks"
	policy, e := netpolicy.New("development", destination, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	_, secret, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}})
	if e != nil {
		t.Fatal(e)
	}
	key, _ := signing.DecodeSecret(secret)
	handler.Store(receiver.Handler([][]byte{key}, receiverLog(), nil, processor))
	if _, e = s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	dbURL, _ := url.Parse(os.Getenv("INTEGRATION_DATABASE_URL"))
	q := dbURL.Query()
	q.Set("search_path", s.DB.Config().ConnConfig.RuntimeParams["search_path"])
	dbURL.RawQuery = q.Encode()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWorkerHelper$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "RF_CRASH_HELPER=1", "RF_CRASH_ACK=1", "RF_CRASH_USE_SENDER=1", "RF_CRASH_DESTINATION="+destination, "RF_CRASH_DB="+dbURL.String())
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	gate := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "CRASH_GATE" {
				gate <- true
				return
			}
		}
		gate <- false
	}()
	select {
	case ok := <-gate:
		if !ok {
			t.Fatal("signed acknowledgment gate missing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("crash gate timed out")
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(6 * time.Second)
	var job *delivery.Job
	for time.Now().Before(deadline) {
		if _, e = s.Recover(ctx, 100); e != nil {
			t.Fatal(e)
		}
		job, e = s.Claim(ctx, security.ID(), time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if job != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job == nil || job.Attempt != 2 {
		t.Fatal("crashed signed request not recovered")
	}
	client := worker.NewClient(policy, time.Second, 1)
	defer client.CloseIdleConnections()
	sender := worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
	result := sender.Send(ctx, *job)
	if result.HTTPStatus != 204 {
		t.Fatal("duplicate not acknowledged", result.HTTPStatus)
	}
	if ok, e := s.Complete(ctx, *job, result); e != nil || !ok {
		t.Fatal(e)
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM receiver_business_events`).Scan(&n); e != nil || n != 1 || requests.Load() != 2 {
		t.Fatal("duplicate business effects", n, requests.Load(), e)
	}
	var id string
	var hash []byte
	if e = s.DB.QueryRow(ctx, `SELECT first_delivery_id,body_sha256 FROM receiver_processed_events`).Scan(&id, &hash); e != nil || id != job.ID || !bytes.Equal(hash, events.Hash(job.Body)) {
		t.Fatal("identity/body changed", e)
	}
	history, _, e := s.ListAttempts(ctx, c.TenantID, job.ID, 100, nil)
	if e != nil || len(history) != 2 || history[0].Outcome != "lease_expired" || history[1].Outcome != "success" {
		t.Fatal("attempt history lost", e)
	}
}
