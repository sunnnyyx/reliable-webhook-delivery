package pagination

import (
	"encoding/base64"
	"relayforge/internal/security"
	"strings"
	"testing"
	"time"
)

func TestCursorBindsCollectionTenantAndFilter(t *testing.T) {
	tenant, id := security.ID(), security.ID()
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	raw := Encode(tenant, "deliveries", "pending", now, id)
	c, e := Decode(raw, tenant, "deliveries", "pending")
	if e != nil || c.ID != id || !c.Time.Equal(now) {
		t.Fatal("round trip failed")
	}
	for _, args := range [][3]string{{security.ID(), "deliveries", "pending"}, {tenant, "events", "pending"}, {tenant, "deliveries", "delivered"}} {
		if _, e = Decode(raw, args[0], args[1], args[2]); e == nil {
			t.Fatal("cursor scope substitution")
		}
	}
	b, _ := base64.RawURLEncoding.DecodeString(raw)
	for _, bad := range []string{"", strings.Repeat("x", 1025), raw + "=", base64.RawURLEncoding.EncodeToString(append(b, []byte(" junk")...)), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(b), `"v":1`, `"v":1,"v":1`, 1)))} {
		if _, e = Decode(bad, tenant, "deliveries", "pending"); e == nil {
			t.Fatal("malformed cursor accepted")
		}
	}
}
