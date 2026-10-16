//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"relayforge/internal/endpoints"
	"relayforge/internal/httpapi"
	"relayforge/internal/migrate"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *postgres.Store {
	t.Helper()
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Fatal("INTEGRATION_DATABASE_URL required")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	schema := "rf_test_" + strings.ReplaceAll(security.ID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	p, e := migrate.Provider(db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal(e)
	}
	vault, e := security.NewVault(bytes.Repeat([]byte{23}, 32))
	if e != nil {
		t.Fatal(e)
	}
	// Earlier contention tests deliberately reserve many leases without dispatching.
	return &postgres.Store{DB: pool, Vault: vault, MaxTenantInFlight: 128}
}

type apiClient struct {
	url    string
	client *http.Client
}

func (a apiClient) request(method, path, token, body string) (int, []byte, error) {
	r, e := http.NewRequest(method, a.url+path, strings.NewReader(body))
	if e != nil {
		return 0, nil, e
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
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
func expect(t *testing.T, a apiClient, method, path, token, body string, want int) []byte {
	t.Helper()
	status, b, e := a.request(method, path, token, body)
	if e != nil || status != want {
		t.Fatalf("%s %s: status %d want %d (transport error=%v)", method, path, status, want, e)
	}
	return b
}
func credentials(t *testing.T, s *postgres.Store, name string) postgres.Credential {
	t.Helper()
	c, e := s.Provision(context.Background(), name)
	if e != nil {
		t.Fatal("provision failed")
	}
	return c
}
func create(t *testing.T, a apiClient, token string) (endpoints.Endpoint, string) {
	t.Helper()
	b := expect(t, a, "POST", "/v1/endpoints", token, `{"url":"https://hooks.example.com/order","subscribed_event_types":["order.created"]}`, 201)
	var result struct {
		Endpoint endpoints.Endpoint `json:"endpoint"`
		Secret   string             `json:"signing_secret"`
	}
	if e := json.Unmarshal(b, &result); e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(result.Secret, "whsec_") {
		t.Fatal("missing creation secret")
	}
	return result.Endpoint, result.Secret
}
func TestEndpointAuthenticationIsolationAndSecrets(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	ctx := context.Background()
	first := credentials(t, s, "Tenant A")
	second := credentials(t, s, "Tenant B")
	expect(t, a, "GET", "/v1/endpoints", "", "", 401)
	expect(t, a, "GET", "/v1/endpoints", "invalid", "", 401)
	ep, secret := create(t, a, first.APIKey)
	other, _ := create(t, a, second.APIKey)
	for _, tc := range []struct{ method, path, body string }{{"GET", "/v1/endpoints/" + other.ID, ""}, {"PATCH", "/v1/endpoints/" + other.ID, `{"enabled":false}`}, {"DELETE", "/v1/endpoints/" + other.ID, ""}, {"POST", "/v1/endpoints/" + other.ID + "/rotate-secret", `{}`}} {
		expect(t, a, tc.method, tc.path, first.APIKey, tc.body, 404)
	}
	for _, path := range []string{"/v1/endpoints", "/v1/endpoints/" + ep.ID} {
		b := expect(t, a, "GET", path, first.APIKey, "", 200)
		if bytes.Contains(b, []byte("whsec_")) || bytes.Contains(b, []byte("encrypted_secret")) || bytes.Contains(b, []byte(other.ID)) {
			t.Fatal("metadata leaks secret or another tenant")
		}
	}
	var hash []byte
	var prefix string
	if e := s.DB.QueryRow(ctx, "SELECT key_hash,key_prefix FROM api_keys WHERE id=$1", first.KeyID).Scan(&hash, &prefix); e != nil || !bytes.Equal(hash, security.Hash(first.APIKey)) || prefix == first.APIKey {
		t.Fatal("API key storage invalid")
	}
	var id string
	var cipher, nonce []byte
	var version int
	if e := s.DB.QueryRow(ctx, "SELECT id,encrypted_secret,nonce,encryption_key_version FROM endpoint_secrets WHERE endpoint_id=$1", ep.ID).Scan(&id, &cipher, &nonce, &version); e != nil {
		t.Fatal(e)
	}
	plain, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Vault.Decrypt(first.TenantID, ep.ID, id, cipher, nonce)
	if e != nil || !bytes.Equal(got, plain) || bytes.Equal(cipher, plain) || version != 1 {
		t.Fatal("encrypted storage round trip failed")
	}
	if _, e = s.Vault.Decrypt(second.TenantID, ep.ID, id, cipher, nonce); e == nil {
		t.Fatal("AAD tenant substitution accepted")
	}
	_, e = s.DB.Exec(ctx, "INSERT INTO endpoint_secrets(id,tenant_id,endpoint_id,encrypted_secret,nonce,encryption_key_version) VALUES($1,$2,$3,$4,$5,1)", security.ID(), second.TenantID, ep.ID, make([]byte, 48), make([]byte, 12))
	var pgErr *pgconn.PgError
	if !errors.As(e, &pgErr) || pgErr.Code != "23503" {
		t.Fatal("cross-tenant foreign key did not reject")
	}
	expect(t, a, "PATCH", "/v1/endpoints/"+ep.ID, first.APIKey, `{"url":"https://new.example.com/path","enabled":false,"subscribed_event_types":["*"]}`, 200)
	v, e := s.GetEndpoint(ctx, first.TenantID, ep.ID)
	if e != nil || v.Enabled || v.URL != "https://new.example.com/path" || len(v.Subscriptions) != 1 || v.Subscriptions[0] != "*" {
		t.Fatal("patch not persisted")
	}
	replacement, e := s.CreateKey(ctx, first.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeKey(ctx, second.TenantID, first.KeyID); !errors.Is(e, endpoints.ErrNotFound) {
		t.Fatal("cross-tenant revoke")
	}
	if e = s.RevokeKey(ctx, first.TenantID, first.KeyID); e != nil {
		t.Fatal(e)
	}
	expect(t, a, "GET", "/v1/endpoints", first.APIKey, "", 401)
	expect(t, a, "GET", "/v1/endpoints", replacement.APIKey, "", 200)
	expect(t, a, "DELETE", "/v1/endpoints/"+ep.ID, replacement.APIKey, "", 204)
	expect(t, a, "GET", "/v1/endpoints/"+ep.ID, replacement.APIKey, "", 404)
	expect(t, a, "PATCH", "/v1/endpoints/"+ep.ID, replacement.APIKey, `{"enabled":true}`, 404)
	expect(t, a, "POST", "/v1/endpoints/"+ep.ID+"/rotate-secret", replacement.APIKey, `{}`, 404)
	var deleted bool
	if e = s.DB.QueryRow(ctx, "SELECT deleted_at IS NOT NULL AND NOT enabled FROM endpoints WHERE id=$1", ep.ID).Scan(&deleted); e != nil || !deleted {
		t.Fatal("not soft deleted")
	}
	if e = s.DisableTenant(ctx, first.TenantID); e != nil {
		t.Fatal(e)
	}
	expect(t, a, "GET", "/v1/endpoints", replacement.APIKey, "", 401)
}
func TestEndpointPaginationValidationAndAtomicCreation(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	c := credentials(t, s, "Pagination")
	other := credentials(t, s, "Other")
	ep, _ := create(t, a, c.APIKey)
	ep2, _ := create(t, a, c.APIKey)
	ctx := context.Background()
	_, e := s.DB.Exec(ctx, "UPDATE endpoints SET created_at='2026-10-15 00:00:00+00' WHERE tenant_id=$1", c.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	b := expect(t, a, "GET", "/v1/endpoints?limit=1", c.APIKey, "", 200)
	var page struct {
		Data []endpoints.Endpoint `json:"data"`
		Next string               `json:"next_cursor"`
	}
	if e = json.Unmarshal(b, &page); e != nil || len(page.Data) != 1 || page.Next == "" {
		t.Fatal("first page invalid")
	}
	firstID := page.Data[0].ID
	b = expect(t, a, "GET", "/v1/endpoints?limit=1&cursor="+page.Next, c.APIKey, "", 200)
	expect(t, a, "GET", "/v1/endpoints?cursor="+page.Next, other.APIKey, "", 400)
	if e = json.Unmarshal(b, &page); e != nil || len(page.Data) != 1 || page.Data[0].ID == firstID || page.Next != "" {
		t.Fatal("second page invalid")
	}
	if page.Data[0].ID != ep.ID && page.Data[0].ID != ep2.ID {
		t.Fatal("unexpected ID")
	}
	for _, q := range []string{"?limit=1%GG", "?cursor=%", "?limit=0", "?limit=101", "?cursor=broken", "?limit=1&limit=2", "?unknown=yes"} {
		expect(t, a, "GET", "/v1/endpoints"+q, c.APIKey, "", 400)
	}
	for _, body := range []string{`{"url":"http://hooks.example.com","subscribed_event_types":["*"]}`, `{"url":"https://127.0.0.1","subscribed_event_types":["*"]}`, `{"url":"https://hooks.example.com","subscribed_event_types":["*","a"]}`, `{"url":"https://hooks.example.com","subscribed_event_types":["a","a"]}`, `{"tenant_id":"spoof","url":"https://hooks.example.com","subscribed_event_types":["*"]}`, `{"url":"x","url":"y"}`} {
		expect(t, a, "POST", "/v1/endpoints", c.APIKey, body, 400)
	}
	expect(t, a, "PATCH", "/v1/endpoints/"+ep.ID, c.APIKey, `{}`, 400)
	expect(t, a, "PATCH", "/v1/endpoints/"+ep.ID, c.APIKey, `{"enabled":null}`, 400)
	expect(t, a, "POST", "/v1/endpoints", c.APIKey, `{"url":"`+strings.Repeat("x", 256<<10)+`"}`, 413)
	expect(t, a, "GET", "/v1/unknown", c.APIKey, "", 404)
	// Force the second insert to fail: endpoint and initial secret must roll back together.
	if _, e = s.DB.Exec(ctx, "ALTER TABLE endpoint_secrets ADD CONSTRAINT reject_new_secrets CHECK (false) NOT VALID"); e != nil {
		t.Fatal(e)
	}
	b = expect(t, a, "POST", "/v1/endpoints", c.APIKey, `{"url":"https://hooks.example.com","subscribed_event_types":["*"]}`, 503)
	if bytes.Contains(b, []byte("reject_new_secrets")) {
		t.Fatal("SQL detail leaked")
	}
	var count int
	if e = s.DB.QueryRow(ctx, "SELECT count(*) FROM endpoints WHERE tenant_id=$1", c.TenantID).Scan(&count); e != nil || count != 2 {
		t.Fatal("partial endpoint insert survived rollback")
	}
}
func TestConcurrentEndpointLimitAndRotation(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	c := credentials(t, s, "Concurrent")
	start := make(chan struct{})
	results := make(chan int, 24)
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			status, _, e := a.request("POST", "/v1/endpoints", c.APIKey, `{"url":"https://hooks.example.com","subscribed_event_types":["*"]}`)
			if e != nil {
				status = 0
			}
			results <- status
		}()
	}
	close(start)
	group.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[201] != 20 || counts[409] != 4 {
		t.Fatalf("unexpected status counts: %v", counts)
	}
	items, _, e := s.ListEndpoints(context.Background(), c.TenantID, 25, nil)
	if e != nil || len(items) != 20 {
		t.Fatal("limit invariant failed")
	}
	id := items[0].ID
	start = make(chan struct{})
	results = make(chan int, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			status, _, e := a.request("POST", "/v1/endpoints/"+id+"/rotate-secret", c.APIKey, `{}`)
			if e != nil {
				status = 0
			}
			results <- status
		}()
	}
	close(start)
	group.Wait()
	close(results)
	counts = map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[201] != 1 || counts[409] != 7 {
		t.Fatalf("rotation counts: %v", counts)
	}
	var active, retiring int
	e = s.DB.QueryRow(context.Background(), "SELECT count(*),count(*) FILTER(WHERE retire_after IS NOT NULL) FROM endpoint_secrets WHERE endpoint_id=$1 AND (retire_after IS NULL OR retire_after>now())", id).Scan(&active, &retiring)
	if e != nil || active != 2 || retiring != 1 {
		t.Fatal("rotation overlap invalid")
	}
	var overlapSeconds float64
	if e = s.DB.QueryRow(context.Background(), `SELECT extract(epoch from(old.retire_after-newkey.active_from)) FROM endpoint_secrets old JOIN endpoint_secrets newkey ON newkey.endpoint_id=old.endpoint_id WHERE old.endpoint_id=$1 AND old.retire_after IS NOT NULL AND newkey.retire_after IS NULL`, id).Scan(&overlapSeconds); e != nil || overlapSeconds != 86400 {
		t.Fatal("overlap is not exactly 24 hours")
	}
	// Advance the recorded old-key window without sleeping for a day.
	if _, e = s.DB.Exec(context.Background(), "UPDATE endpoint_secrets SET active_from=now()-interval '48 hours',retire_after=now()-interval '1 hour' WHERE endpoint_id=$1 AND retire_after IS NOT NULL", id); e != nil {
		t.Fatal(e)
	}
	expect(t, a, "POST", "/v1/endpoints/"+id+"/rotate-secret", c.APIKey, `{}`, 201)
	var history int
	if e = s.DB.QueryRow(context.Background(), "SELECT count(*) FROM endpoint_secrets WHERE endpoint_id=$1", id).Scan(&history); e != nil || history != 3 {
		t.Fatal("retired secret history missing")
	}
	expect(t, a, "DELETE", "/v1/endpoints/"+id, c.APIKey, "", 204)
	create(t, a, c.APIKey)
}

func TestAuthUnavailableStorageReturns503(t *testing.T) {
	s := testStore(t)
	token, _, e := security.Token()
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	a := apiClient{server.URL, server.Client()}
	body := expect(t, a, "GET", "/v1/endpoints", token, "", 503)
	var failure map[string]string
	if e = json.Unmarshal(body, &failure); e != nil || failure["code"] != "storage_unavailable" || failure["request_id"] == "" {
		t.Fatal("invalid storage error")
	}
	if bytes.Contains(body, []byte(token)) {
		t.Fatal("credential leaked")
	}
}
