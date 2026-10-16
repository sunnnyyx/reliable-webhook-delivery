//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/httpapi"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const orderBody = `{"type":"order.created","data":{"order_id":"42","nested":{"ok":true}}}`

func eventRequest(a apiClient, token, key, raw string) (int, []byte, error) {
	r, e := http.NewRequest("POST", a.url+"/v1/events", strings.NewReader(raw))
	if e != nil {
		return 0, nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, e := a.client.Do(r)
	if e != nil {
		return 0, nil, e
	}
	defer response.Body.Close()
	b, e := io.ReadAll(response.Body)
	if response.Header.Get("X-Request-ID") == "" {
		return 0, nil, errors.New("missing request ID")
	}
	return response.StatusCode, b, e
}
func submit(t *testing.T, a apiClient, token, key, raw string, want int) []byte {
	t.Helper()
	status, b, e := eventRequest(a, token, key, raw)
	if e != nil || status != want {
		t.Fatalf("ingestion status %d want %d, transport error %v", status, want, e)
	}
	return b
}
func accepted(t *testing.T, b []byte) events.Acceptance {
	t.Helper()
	var v events.Acceptance
	if e := json.Unmarshal(b, &v); e != nil || !security.ValidID(v.EventID) {
		t.Fatal("invalid acceptance response")
	}
	return v
}
func counts(t *testing.T, s *postgres.Store, tenant string, wantEvents, wantDeliveries, wantKeys int) {
	t.Helper()
	var e, d, k int
	if err := s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM events WHERE tenant_id=$1),(SELECT count(*) FROM deliveries WHERE tenant_id=$1),(SELECT count(*) FROM ingestion_idempotency WHERE tenant_id=$1)`, tenant).Scan(&e, &d, &k); err != nil || e != wantEvents || d != wantDeliveries || k != wantKeys {
		t.Fatalf("row counts events=%d deliveries=%d keys=%d, error=%v", e, d, k, err)
	}
}
func targets(t *testing.T, s *postgres.Store, tenant, kind string) endpoints.Endpoint {
	t.Helper()
	v, _, e := s.CreateEndpoint(context.Background(), tenant, endpoints.Create{URL: "https://hooks.example.com/" + kind, Subscriptions: []string{kind}})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func expectPG(t *testing.T, e error, code string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(e, &pg) || pg.Code != code {
		t.Fatalf("expected PostgreSQL code %s, got %v", code, e)
	}
}
func TestIngestionFanoutSnapshotsAndTenantViews(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	ctx := context.Background()
	one := credentials(t, s, "Publisher A")
	two := credentials(t, s, "Publisher B")
	zero := credentials(t, s, "No endpoints")
	exact := targets(t, s, one.TenantID, "order.created")
	wild := targets(t, s, one.TenantID, "*")
	targets(t, s, one.TenantID, "payment.created")
	disabled := targets(t, s, one.TenantID, "order.created")
	no := false
	if _, e := s.UpdateEndpoint(ctx, one.TenantID, disabled.ID, endpoints.Patch{Enabled: &no}); e != nil {
		t.Fatal(e)
	}
	deleted := targets(t, s, one.TenantID, "order.created")
	if e := s.DeleteEndpoint(ctx, one.TenantID, deleted.ID); e != nil {
		t.Fatal(e)
	}
	other := targets(t, s, two.TenantID, "order.created")
	response := submit(t, a, one.APIKey, "order-42", orderBody, 202)
	v := accepted(t, response)
	if v.DeliveryCount != 2 {
		t.Fatal("incorrect exact/wildcard fan-out")
	}
	counts(t, s, one.TenantID, 1, 2, 1)
	b := expect(t, a, "GET", "/v1/events/"+v.EventID, one.APIKey, "", 200)
	var detail events.Detail
	if e := json.Unmarshal(b, &detail); e != nil || detail.DeliverySummary["pending"] != 2 {
		t.Fatal("event detail invalid")
	}
	var stored []byte
	if e := s.DB.QueryRow(ctx, "SELECT outgoing_body FROM events WHERE tenant_id=$1 AND id=$2", one.TenantID, v.EventID).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(events.Hash(stored), mustHash(t, s, v.EventID)) {
		t.Fatal("body hash mismatch")
	}
	if !detail.Event.CreatedAt.Equal(v.AcceptedAt) || detail.Event.ExpiresAt.Sub(v.AcceptedAt) != 30*24*time.Hour {
		t.Fatal("event timestamps/retention")
	}
	var envelope map[string]json.RawMessage
	if e := json.Unmarshal(stored, &envelope); e != nil || !bytes.Contains(envelope["data"], []byte("order_id")) {
		t.Fatal("outgoing envelope invalid")
	}
	deliveries, _, e := s.ListDeliveries(ctx, one.TenantID, 25, nil, delivery.Filters{EventID: v.EventID})
	if e != nil || len(deliveries) != 2 {
		t.Fatal("delivery fanout missing")
	}
	var exactDelivery delivery.Delivery
	for _, d := range deliveries {
		if d.EndpointID == exact.ID {
			exactDelivery = d
		}
		if d.Status != "pending" || d.CycleNumber != 1 || d.CycleAttemptCount != 0 || d.LifetimeAttemptCount != 0 || !d.NextAttemptAt.Equal(v.AcceptedAt) {
			t.Fatal("initial delivery state invalid")
		}
		expect(t, a, "GET", "/v1/deliveries/"+d.ID, two.APIKey, "", 404)
	}
	expect(t, a, "GET", "/v1/events/"+v.EventID, two.APIKey, "", 404)
	expect(t, a, "GET", "/v1/deliveries/"+exactDelivery.ID, one.APIKey, "", 200)
	newURL := "https://new.example.com/orders"
	subs := []string{"payment.created"}
	if _, e = s.UpdateEndpoint(ctx, one.TenantID, exact.ID, endpoints.Patch{URL: &newURL, Subscriptions: &subs}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.UpdateEndpoint(ctx, one.TenantID, wild.ID, endpoints.Patch{Enabled: &no}); e != nil {
		t.Fatal(e)
	}
	targets(t, s, one.TenantID, "order.created")
	if b = submit(t, a, one.APIKey, "order-42", orderBody, 202); !bytes.Equal(b, response) {
		t.Fatal("stored response changed after endpoint changes")
	}
	next := accepted(t, submit(t, a, one.APIKey, "order-43", orderBody, 202))
	if next.DeliveryCount != 1 {
		t.Fatal("future subscription snapshot incorrect")
	}
	d, e := s.GetDelivery(ctx, one.TenantID, exactDelivery.ID)
	if e != nil || d.URLSnapshot == newURL || d.URLSnapshot != exact.URL {
		t.Fatal("old destination changed")
	}
	var again []byte
	if e = s.DB.QueryRow(ctx, "SELECT outgoing_body FROM events WHERE id=$1", v.EventID).Scan(&again); e != nil || !bytes.Equal(stored, again) {
		t.Fatal("stored event bytes changed")
	}
	if e = s.DeleteEndpoint(ctx, one.TenantID, exact.ID); e != nil {
		t.Fatal(e)
	}
	d, e = s.GetDelivery(ctx, one.TenantID, exactDelivery.ID)
	if e != nil || d.Status != "cancelled" || d.LastErrorCode == nil || *d.LastErrorCode != "endpoint_deleted" {
		t.Fatal("deletion did not cancel pending work")
	}
	b = expect(t, a, "GET", "/v1/deliveries?status=cancelled&event_id="+v.EventID+"&endpoint_id="+exact.ID, one.APIKey, "", 200)
	var page struct {
		Data []delivery.Delivery `json:"data"`
	}
	if json.Unmarshal(b, &page) != nil || len(page.Data) != 1 {
		t.Fatal("delivery filters incorrect")
	}
	otherEvent := accepted(t, submit(t, a, two.APIKey, "order-42", strings.Replace(orderBody, "42", "99", 1), 202))
	if otherEvent.DeliveryCount != 1 {
		t.Fatal("idempotency namespace not tenant scoped")
	}
	expect(t, a, "GET", "/v1/events/"+otherEvent.EventID, one.APIKey, "", 404)
	b = expect(t, a, "GET", "/v1/deliveries?event_id="+v.EventID, two.APIKey, "", 200)
	if json.Unmarshal(b, &page) != nil || len(page.Data) != 0 {
		t.Fatal("filter leaked another tenant")
	}
	emptyResponse := submit(t, a, zero.APIKey, "zero", orderBody, 202)
	empty := accepted(t, emptyResponse)
	if empty.DeliveryCount != 0 {
		t.Fatal("zero fanout rejected")
	}
	counts(t, s, zero.TenantID, 1, 0, 1)
	targets(t, s, zero.TenantID, "order.created")
	if b = submit(t, a, zero.APIKey, "zero", orderBody, 202); !bytes.Equal(b, emptyResponse) {
		t.Fatal("late endpoint received historical event")
	}
	counts(t, s, zero.TenantID, 1, 0, 1)
	_, e = s.DB.Exec(ctx, "UPDATE events SET outgoing_body=$2 WHERE id=$1", v.EventID, []byte(`{}`))
	expectPG(t, e, "23514")
	_, e = s.DB.Exec(ctx, "UPDATE deliveries SET url_snapshot=$2 WHERE id=$1", exactDelivery.ID, newURL)
	expectPG(t, e, "23514")
	insert := `INSERT INTO deliveries(id,tenant_id,event_id,endpoint_id,url_snapshot,next_attempt_at,cycle_started_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,now(),now(),now(),now())`
	_, e = s.DB.Exec(ctx, insert, security.ID(), one.TenantID, otherEvent.EventID, exact.ID, exact.URL)
	expectPG(t, e, "23503")
	_, e = s.DB.Exec(ctx, insert, security.ID(), one.TenantID, v.EventID, other.ID, other.URL)
	expectPG(t, e, "23503")
	_, e = s.DB.Exec(ctx, insert, security.ID(), one.TenantID, v.EventID, exact.ID, exact.URL)
	expectPG(t, e, "23505")
}
func mustHash(t *testing.T, s *postgres.Store, id string) []byte {
	t.Helper()
	var b []byte
	if e := s.DB.QueryRow(context.Background(), "SELECT body_sha256 FROM events WHERE id=$1", id).Scan(&b); e != nil {
		t.Fatal(e)
	}
	return b
}
func TestConcurrentIdempotencyAndLostResponse(t *testing.T) {
	s := testStore(t)
	one := credentials(t, s, "Concurrent publisher")
	targets(t, s, one.TenantID, "order.created")
	targets(t, s, one.TenantID, "*")
	handler := httpapi.API(s, time.Second)
	server := httptest.NewServer(handler)
	defer server.Close()
	second := httptest.NewServer(httpapi.API(&postgres.Store{DB: s.DB, Vault: s.Vault}, time.Second))
	defer second.Close()
	clients := []apiClient{{server.URL, server.Client()}, {second.URL, second.Client()}}
	start := make(chan struct{})
	type result struct {
		status int
		body   []byte
		err    error
	}
	out := make(chan result, 10)
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			status, b, e := eventRequest(clients[i%2], one.APIKey, "concurrent", orderBody)
			out <- result{status, b, e}
		}(i)
	}
	close(start)
	group.Wait()
	close(out)
	var first []byte
	for r := range out {
		if r.err != nil || r.status != 202 {
			t.Fatal("concurrent request failed")
		}
		if first == nil {
			first = r.body
		} else if !bytes.Equal(first, r.body) {
			t.Fatal("concurrent responses differ")
		}
	}
	counts(t, s, one.TenantID, 1, 2, 1)
	submit(t, clients[0], one.APIKey, "concurrent", " "+orderBody, 409)
	submit(t, clients[0], one.APIKey, "concurrent", `{"data":{"order_id":"42","nested":{"ok":true}},"type":"order.created"}`, 409)
	counts(t, s, one.TenantID, 1, 2, 1)
	// A deterministic failing ResponseWriter loses the first response after commit.
	writer := &lostWriter{headers: http.Header{}}
	r := httptest.NewRequest("POST", "/v1/events", strings.NewReader(orderBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+one.APIKey)
	r.Header.Set("Idempotency-Key", "lost-response")
	handler.ServeHTTP(writer, r)
	if writer.status != 202 || writer.writes != 1 {
		t.Fatal("response failpoint not reached")
	}
	counts(t, s, one.TenantID, 2, 4, 2)
	var saved []byte
	if e := s.DB.QueryRow(context.Background(), "SELECT response_body FROM ingestion_idempotency WHERE tenant_id=$1 AND idempotency_key='lost-response'", one.TenantID).Scan(&saved); e != nil {
		t.Fatal(e)
	}
	retry := submit(t, clients[0], one.APIKey, "lost-response", orderBody, 202)
	if !bytes.Equal(saved, retry) {
		t.Fatal("lost-response retry changed result")
	}
	counts(t, s, one.TenantID, 2, 4, 2)
}

type lostWriter struct {
	headers        http.Header
	status, writes int
}

func (w *lostWriter) Header() http.Header         { return w.headers }
func (w *lostWriter) WriteHeader(status int)      { w.status = status }
func (w *lostWriter) Write(b []byte) (int, error) { w.writes++; return 0, io.ErrClosedPipe }
func TestExpiredIdempotencyKeyReplacedOnce(t *testing.T) {
	s := testStore(t)
	c := credentials(t, s, "Expiry publisher")
	targets(t, s, c.TenantID, "order.created")
	var nanos atomic.Int64
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	nanos.Store(base.UnixNano())
	s.Now = func() time.Time { return time.Unix(0, nanos.Load()) }
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	old := accepted(t, submit(t, a, c.APIKey, "expiring", orderBody, 202))
	nanos.Store(base.Add(6 * 24 * time.Hour).UnixNano())
	if repeated := submit(t, a, c.APIKey, "expiring", orderBody, 202); accepted(t, repeated).EventID != old.EventID {
		t.Fatal("unexpired key changed identity")
	}
	var originalExpiry time.Time
	if e := s.DB.QueryRow(context.Background(), "SELECT expires_at FROM ingestion_idempotency WHERE tenant_id=$1", c.TenantID).Scan(&originalExpiry); e != nil || !originalExpiry.Equal(base.Add(7*24*time.Hour)) {
		t.Fatal("duplicate request extended idempotency TTL")
	}
	nanos.Store(base.Add(7 * 24 * time.Hour).UnixNano())
	freshRaw := strings.Replace(orderBody, "42", "84", 1)
	start := make(chan struct{})
	out := make(chan []byte, 10)
	errs := make(chan error, 10)
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			status, b, e := eventRequest(a, c.APIKey, "expiring", freshRaw)
			if e != nil || status != 202 {
				errs <- errors.New("expiry contention request failed")
				return
			}
			out <- b
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	close(out)
	var response []byte
	for b := range out {
		if response == nil {
			response = b
		} else if !bytes.Equal(response, b) {
			t.Fatal("multiple expired-key responses")
		}
	}
	fresh := accepted(t, response)
	if fresh.EventID == old.EventID || !fresh.AcceptedAt.Equal(base.Add(7*24*time.Hour)) {
		t.Fatal("expired key did not produce one fresh event")
	}
	counts(t, s, c.TenantID, 2, 2, 1)
	submit(t, a, c.APIKey, "expiring", orderBody, 409)
	var created, expires time.Time
	if e := s.DB.QueryRow(context.Background(), "SELECT created_at,expires_at FROM ingestion_idempotency WHERE tenant_id=$1", c.TenantID).Scan(&created, &expires); e != nil || expires.Sub(created) != 7*24*time.Hour {
		t.Fatal("replacement TTL incorrect")
	}
}
func TestIngestionRollbackAndDeferredCompleteness(t *testing.T) {
	s := testStore(t)
	c := credentials(t, s, "Rollback publisher")
	targets(t, s, c.TenantID, "order.created")
	targets(t, s, c.TenantID, "*")
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	ctx := context.Background()
	if _, e := s.DB.Exec(ctx, "ALTER TABLE deliveries ADD CONSTRAINT reject_new_deliveries CHECK(false) NOT VALID"); e != nil {
		t.Fatal(e)
	}
	b := submit(t, a, c.APIKey, "retry-after-storage", orderBody, 503)
	if !bytes.Contains(b, []byte("same Idempotency-Key")) {
		t.Fatal("storage failure omitted safe retry instruction")
	}
	if bytes.Contains(b, []byte("reject_new_deliveries")) {
		t.Fatal("SQL detail leaked")
	}
	counts(t, s, c.TenantID, 0, 0, 0)
	if _, e := s.DB.Exec(ctx, "ALTER TABLE deliveries DROP CONSTRAINT reject_new_deliveries"); e != nil {
		t.Fatal(e)
	}
	_, e := s.DB.Exec(ctx, `CREATE FUNCTION forced_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test commit failure' USING ERRCODE='23514'; END; $$; CREATE CONSTRAINT TRIGGER fail_commit AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION forced_commit_failure()`)
	if e != nil {
		t.Fatal(e)
	}
	submit(t, a, c.APIKey, "retry-after-storage", orderBody, 503)
	counts(t, s, c.TenantID, 0, 0, 0)
	if _, e = s.DB.Exec(ctx, "DROP TRIGGER fail_commit ON events; DROP FUNCTION forced_commit_failure()"); e != nil {
		t.Fatal(e)
	}
	v := accepted(t, submit(t, a, c.APIKey, "retry-after-storage", orderBody, 202))
	if v.DeliveryCount != 2 {
		t.Fatal("retry did not recover")
	}
	counts(t, s, c.TenantID, 1, 2, 1)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `INSERT INTO ingestion_idempotency(tenant_id,idempotency_key,request_hash,event_id,created_at,expires_at) VALUES($1,'incomplete',$2,$3,now(),now()+interval '7 days')`, c.TenantID, events.Hash([]byte(orderBody)), v.EventID)
	if e != nil {
		t.Fatal(e)
	}
	expectPG(t, tx.Commit(ctx), "23514")
	counts(t, s, c.TenantID, 1, 2, 1)
}
func TestIngestionRequestValidationAndPagination(t *testing.T) {
	s := testStore(t)
	c := credentials(t, s, "Validation publisher")
	other := credentials(t, s, "Other publisher")
	targets(t, s, c.TenantID, "order.created")
	targets(t, s, c.TenantID, "*")
	s.Now = func() time.Time { return time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC) }
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	submit(t, a, "", "valid", orderBody, 401)
	for _, key := range []string{"", strings.Repeat("x", 129), "unicode-ключ"} {
		submit(t, a, c.APIKey, key, orderBody, 400)
	}
	for _, raw := range []string{`{}`, `{"type":"*","data":{}}`, `{"type":"order.created","data":{"x":1,"x":2}}`, `{"type":"order.created","data":{}} {}`, `{"type":"order.created","data":{},"tenant_id":"spoof"}`} {
		submit(t, a, c.APIKey, "invalid", raw, 400)
	}
	submit(t, a, c.APIKey, "oversized", `{"type":"order.created","data":"`+strings.Repeat("x", events.MaxInputBytes)+`"}`, 413)
	r, _ := http.NewRequest("POST", server.URL+"/v1/events", strings.NewReader(orderBody))
	r.Header.Set("Authorization", "Bearer "+c.APIKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("Idempotency-Key", "one")
	r.Header.Add("Idempotency-Key", "two")
	res, e := a.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal("duplicate keys accepted")
	}
	counts(t, s, c.TenantID, 0, 0, 0)
	for _, key := range []string{"page-a", "page-b", "page-c"} {
		submit(t, a, c.APIKey, key, orderBody, 202)
	}
	for _, resource := range []string{"events", "deliveries"} {
		var firstCursor string
		seen := map[string]bool{}
		path := "/v1/" + resource + "?limit=1"
		for pageNo := 0; pageNo < 10; pageNo++ {
			b := expect(t, a, "GET", path, c.APIKey, "", 200)
			var page struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
				Next string `json:"next_cursor"`
			}
			if json.Unmarshal(b, &page) != nil || len(page.Data) != 1 || seen[page.Data[0].ID] {
				t.Fatal("pagination duplicate or omission")
			}
			seen[page.Data[0].ID] = true
			if page.Next == "" {
				break
			}
			if firstCursor == "" {
				firstCursor = page.Next
			}
			path = "/v1/" + resource + "?limit=1&cursor=" + page.Next
		}
		want := 3
		if resource == "deliveries" {
			want = 6
		}
		if len(seen) != want {
			t.Fatal("pagination did not visit every row")
		}
		expect(t, a, "GET", "/v1/"+resource+"?cursor="+firstCursor, other.APIKey, "", 400)
		otherResource := "events"
		if resource == "events" {
			otherResource = "deliveries"
		}
		expect(t, a, "GET", "/v1/"+otherResource+"?cursor="+firstCursor, c.APIKey, "", 400)
		if resource == "deliveries" {
			expect(t, a, "GET", "/v1/deliveries?status=pending&cursor="+firstCursor, c.APIKey, "", 400)
		}
	}
	for _, path := range []string{"/v1/events?limit=101", "/v1/events?limit=1%GG", "/v1/events?cursor=broken", "/v1/events?status=pending", "/v1/deliveries?status=unknown", "/v1/deliveries?status=", "/v1/deliveries?event_id=bad", "/v1/deliveries?limit=0", "/v1/deliveries?limit=1&limit=2"} {
		expect(t, a, "GET", path, c.APIKey, "", 400)
	}
	expect(t, a, "GET", "/v1/events/"+security.ID(), c.APIKey, "", 404)
	expect(t, a, "GET", "/v1/deliveries/invalid", c.APIKey, "", 404)
}
func TestAcceptanceAndDeletionCoordinateWithoutOrphans(t *testing.T) {
	s := testStore(t)
	c := credentials(t, s, "Lifecycle publisher")
	ep := targets(t, s, c.TenantID, "order.created")
	gated := *s
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	gated.NewID = func() string {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return security.ID()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acceptDone := make(chan error, 1)
	go func() { _, e := gated.Accept(ctx, c.TenantID, "accept-delete", []byte(orderBody)); acceptDone <- e }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("acceptance did not reach locked endpoint")
	}
	cfg := s.DB.Config().Copy()
	cfg.MaxConns = 1
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	var pid int
	if e = pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	deleting := &postgres.Store{DB: pool, Vault: s.Vault}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- deleting.DeleteEndpoint(ctx, c.TenantID, ep.ID) }()
	// Observe the actual PostgreSQL lock wait rather than guessing with a sleep.
	waiting := false
	for !waiting {
		if e = s.DB.QueryRow(ctx, "SELECT coalesce(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1", pid).Scan(&waiting); e != nil {
			unblock()
			t.Fatal(e)
		}
		if !waiting {
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				unblock()
				t.Fatal("delete did not wait on acceptance lock")
			}
		}
	}
	unblock()
	if e = <-acceptDone; e != nil {
		t.Fatal(e)
	}
	if e = <-deleteDone; e != nil {
		t.Fatal(e)
	}
	items, _, e := s.ListDeliveries(ctx, c.TenantID, 25, nil, delivery.Filters{EndpointID: ep.ID})
	if e != nil || len(items) != 1 || items[0].Status != "cancelled" {
		t.Fatal("accepted delivery survived endpoint deletion")
	}
	fresh, e := s.Accept(ctx, c.TenantID, "after-delete", []byte(orderBody))
	if e != nil {
		t.Fatal(e)
	}
	v := accepted(t, fresh.Body)
	if v.DeliveryCount != 0 {
		t.Fatal("deleted endpoint received new delivery")
	}
}
