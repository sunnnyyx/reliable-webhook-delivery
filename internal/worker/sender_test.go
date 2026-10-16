package worker

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"relayforge/internal/delivery"
	"relayforge/internal/netpolicy"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(c context.Context, n, h string) (net.Conn, error) { return f(c, n, h) }
func jobFixture(t *testing.T) (delivery.Job, *security.Vault, []byte) {
	t.Helper()
	vault, e := security.NewVault(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	j := delivery.Job{ID: security.ID(), TenantID: security.ID(), EndpointID: security.ID(), URL: "https://example.com/webhooks", Body: []byte(`{ "order": 42 }`)}
	key := bytes.Repeat([]byte{9}, 32)
	id := security.ID()
	cipher, nonce, e := vault.Encrypt(j.TenantID, j.EndpointID, id, key)
	if e != nil {
		t.Fatal(e)
	}
	j.Secrets = []delivery.Secret{{ID: id, Cipher: cipher, Nonce: nonce, Version: 1}}
	return j, vault, key
}
func TestTransportPinsDNSPreservesTLSAndBlocksRebinding(t *testing.T) {
	j, vault, key := jobFixture(t)
	var requests, lookups, dials atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if r.Host != "example.com" || r.TLS.ServerName != "example.com" || !bytes.Equal(raw, j.Body) || r.Header.Get("Authorization") != "" || r.Header.Get("Baggage") != "" {
			t.Error("hostname/body/header contract changed")
		}
		if e := signing.Verify(j.ID, r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), raw, [][]byte{key}, time.Now(), 5*time.Minute); e != nil {
			t.Error(e)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	p, e := netpolicy.New("production", "", resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) > 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}), dialerFunc(func(ctx context.Context, _, address string) (net.Conn, error) {
		dials.Add(1)
		if address != "8.8.8.8:443" {
			t.Error("hostname dialed instead of vetted IP")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}))
	if e != nil {
		t.Fatal(e)
	}
	client := NewClient(p, time.Second, 1)
	transport := client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = roots
	defer client.CloseIdleConnections()
	if transport.Proxy != nil || !transport.DisableKeepAlives || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("unsafe transport options")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	sender := &Sender{Client: client, Policy: p, Vault: vault}
	if result := sender.Send(context.Background(), j); result.Outcome != "success" {
		t.Fatalf("signed send failed: %s", result.ErrorCode)
	}
	result := sender.Send(context.Background(), j)
	if result.ErrorCode != "forbidden_destination" || result.Outcome != "permanent_failure" || requests.Load() != 1 || dials.Load() != 1 || lookups.Load() != 2 {
		t.Fatal("rebinding was not blocked before dial")
	}
}
func TestRedirectsAndUntrustedCertificatesDoNotSucceed(t *testing.T) {
	j, vault, _ := jobFixture(t)
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer server.Close()
	p, e := netpolicy.New("production", "", resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}), dialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}))
	if e != nil {
		t.Fatal(e)
	}
	client := NewClient(p, time.Second, 1)
	defer client.CloseIdleConnections()
	sender := &Sender{Client: client, Policy: p, Vault: vault}
	if r := sender.Send(context.Background(), j); r.ErrorCode != "tls_validation_failed" || r.Outcome != "permanent_failure" {
		t.Fatal("invalid TLS certificate accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	if r := sender.Send(context.Background(), j); r.ErrorCode != "redirect_blocked" || r.HTTPStatus != 302 || targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestBoundedResponseReadAndSecretFailure(t *testing.T) {
	j, vault, _ := jobFixture(t)
	p, _ := netpolicy.New("production", "", nil, nil)
	body := &countReader{remaining: 1 << 20}
	calls := 0
	sender := &Sender{Policy: p, Vault: vault, Client: doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: body}, nil
	})}
	if r := sender.Send(context.Background(), j); r.Outcome != "success" || body.read != 4096 || !body.closed {
		t.Fatal("response read was not bounded/closed")
	}
	j.TenantID = security.ID()
	if r := sender.Send(context.Background(), j); r.ErrorCode != "secret_unavailable" || calls != 1 {
		t.Fatal("AAD mismatch reached HTTP")
	}
}

type countReader struct {
	remaining, read int
	closed          bool
}

func (r *countReader) Read(b []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(b)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	r.read += n
	return n, nil
}
func (r *countReader) Close() error { r.closed = true; return nil }
