package netpolicy

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
)

// ValidateURL is registration groundwork. It does not authorize outbound dialing;
// the worker must additionally resolve and vet every DNS answer at connection time.
func ValidateURL(raw string) error {
	invalid := errors.New("URL must use HTTPS port 443 and a DNS hostname, without credentials or fragments")
	if len(raw) == 0 || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return invalid
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Host == "" {
		return invalid
	}
	host := strings.ToLower(u.Hostname())
	if _, e = netip.ParseAddr(host); e == nil {
		return invalid
	}
	if u.Port() != "" && u.Port() != "443" {
		return invalid
	}
	if strings.HasSuffix(u.Host, ":") {
		return invalid
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") || !strings.Contains(host, ".") {
		return invalid
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".test", ".invalid", ".example"} {
		if strings.HasSuffix(host, suffix) {
			return invalid
		}
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return invalid
			}
		}
	}
	tld := labels[len(labels)-1]
	if strings.HasPrefix(tld, "xn--") && len(tld) > 4 {
		return nil
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			return invalid
		}
	}
	return nil
}
