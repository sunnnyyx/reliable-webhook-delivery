package netpolicy

import (
	"strings"
	"testing"
)

func TestRegistrationPolicy(t *testing.T) {
	for _, raw := range []string{"https://hooks.example.com/webhooks", "https://hooks.example.com:443/path?a=b", "https://xn--bcher-kva.de/hook"} {
		if e := ValidateURL(raw); e != nil {
			t.Fatalf("valid URL rejected: %s", raw)
		}
	}
	for _, raw := range []string{"http://hooks.example.com", "https://user:pass@hooks.example.com", "https://hooks.example.com/#frag", "https://hooks.example.com/#", "https://hooks.example.com:8443/", "https://hooks.example.com:/", "https://127.0.0.1", "https://169.254.169.254", "https://[::1]", "https://[::ffff:127.0.0.1]", "https://[fe80::1%25eth0]", "https://localhost", "https://foo.localhost", "https://foo.local", "https://foo.internal", "https://2130706433", "https://0177.0.0.1", "https://0x7f000001", "https://0x7f.0x0.0x0.0x1", "https://127.0.0.0x1", "https://good.com.evil.123", "https://foo..com", "https://-foo.com", "https://foo-.com", "https://foo.com.", "https://bücher.de", " https://hooks.example.com", "https://hooks.example.com/" + strings.Repeat("x", 2048)} {
		if ValidateURL(raw) == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
}
