package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

var (
	ErrForbidden = errors.New("destination blocked by network policy")
	ErrDNS       = errors.New("destination DNS lookup failed")
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}
type Policy struct {
	devURL, devPort string
	resolver        Resolver
	dialer          Dialer
}

func New(profile, devURL string, resolver Resolver, dialer Dialer) (*Policy, error) {
	if profile != "development" && profile != "production" {
		return nil, errors.New("invalid network policy profile")
	}
	p := &Policy{resolver: resolver, dialer: dialer}
	if p.resolver == nil {
		p.resolver = net.DefaultResolver
	}
	if p.dialer == nil {
		p.dialer = &net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
	}
	if devURL != "" {
		if profile != "development" {
			return nil, errors.New("DEV_RECEIVER_URL is forbidden in production")
		}
		u, e := url.Parse(devURL)
		if e != nil || u.Scheme != "http" || u.Hostname() != "localhost" || u.User != nil || u.Path != "/webhooks" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
			return nil, errors.New("DEV_RECEIVER_URL must be exactly http://localhost:<port>/webhooks")
		}
		n, e := strconv.Atoi(u.Port())
		if e != nil || n < 1 || n > 65535 || devURL != "http://localhost:"+strconv.Itoa(n)+"/webhooks" {
			return nil, errors.New("DEV_RECEIVER_URL requires a canonical explicit port")
		}
		p.devURL = devURL
		p.devPort = strconv.Itoa(n)
	}
	return p, nil
}
func (p *Policy) ValidateURL(raw string) error {
	if p.devURL != "" && raw == p.devURL {
		return nil
	}
	if e := ValidateURL(raw); e != nil {
		return ErrForbidden
	}
	return nil
}
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrForbidden
	}
	if p.devURL != "" && host == "localhost" && port == p.devPort {
		return p.dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	}
	if port != "443" || ValidateURL("https://"+net.JoinHostPort(host, port)) != nil {
		return nil, ErrForbidden
	}
	addresses, e := p.resolver.LookupNetIP(ctx, "ip", host)
	if e != nil || len(addresses) == 0 {
		return nil, ErrDNS
	}
	for _, ip := range addresses {
		if !PublicIP(ip) {
			return nil, ErrForbidden
		}
	}
	var last error
	for _, ip := range addresses {
		conn, e := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

// Conservative exclusions based on the IANA special-purpose registries. Even
// special-use ranges flagged globally reachable are excluded in this first policy.
var special4 = prefixes("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "168.63.129.16/32")
var special6 = prefixes("2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20")
var global6 = netip.MustParsePrefix("2000::/3")

func prefixes(raw ...string) []netip.Prefix {
	result := []netip.Prefix{}
	for _, s := range raw {
		result = append(result, netip.MustParsePrefix(s))
	}
	return result
}
func PublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	ranges := special4
	if ip.Is6() {
		if !global6.Contains(ip) {
			return false
		}
		ranges = special6
	}
	for _, r := range ranges {
		if r.Contains(ip) {
			return false
		}
	}
	return true
}
