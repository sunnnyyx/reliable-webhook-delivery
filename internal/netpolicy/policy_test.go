package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

type resolveFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolveFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (f dialFunc) DialContext(c context.Context, n, h string) (net.Conn, error) { return f(c, n, h) }
func TestIPPolicy(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8"} {
		if !PublicIP(netip.MustParseAddr(raw)) {
			t.Fatalf("public address blocked: %s", raw)
		}
	}
	for _, raw := range []string{"0.0.0.0", "10.0.0.1", "100.100.100.200", "127.0.0.1", "169.254.169.254", "172.31.0.1", "192.0.0.9", "192.168.1.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "168.63.129.16", "::", "::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2001:db8::1", "2001::1", "2002:7f00:1::", "3fff::1", "fc00::1", "fe80::1", "ff02::1", "fe80::1%en0"} {
		if PublicIP(netip.MustParseAddr(raw)) {
			t.Fatalf("special address allowed: %s", raw)
		}
	}
}
func TestAllAnswersVettedAndOnlyVettedIPDialed(t *testing.T) {
	dialed := []string{}
	d := dialFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, errors.New("test dial stop")
	})
	answers := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}
	p, e := New("production", "", resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) { return answers, nil }), d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(e, ErrForbidden) || len(dialed) != 0 {
		t.Fatal("mixed DNS answers were dialed")
	}
	answers = answers[:1]
	_, _ = p.DialContext(context.Background(), "tcp", "example.com:443")
	if len(dialed) != 1 || dialed[0] != "8.8.8.8:443" {
		t.Fatal("dial did not pin vetted IP")
	}
	answers = []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	if _, e = p.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(e, ErrForbidden) || len(dialed) != 1 {
		t.Fatal("changed DNS answer bypassed policy")
	}
}
func TestDevelopmentExceptionIsExactAndRejectedInProduction(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8090/webhooks", "http://localhost:8090/other", "http://localhost:8090/webhooks?x=y", "http://localhost:08090/webhooks"} {
		if _, e := New("development", raw, nil, nil); e == nil {
			t.Fatal("broad dev exception accepted")
		}
	}
	if _, e := New("production", "http://localhost:8090/webhooks", nil, nil); e == nil {
		t.Fatal("production exception accepted")
	}
	dialed := ""
	p, e := New("development", "http://localhost:8090/webhooks", nil, dialFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("test stop")
	}))
	if e != nil {
		t.Fatal(e)
	}
	if p.ValidateURL("http://localhost:8090/webhooks") != nil || p.ValidateURL("http://localhost:8090/other") == nil {
		t.Fatal("URL exception not exact")
	}
	_, _ = p.DialContext(context.Background(), "tcp", "localhost:8090")
	if dialed != "127.0.0.1:8090" {
		t.Fatal("local receiver was not pinned")
	}
}
