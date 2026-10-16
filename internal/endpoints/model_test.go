package endpoints

import (
	"encoding/base64"
	"relayforge/internal/security"
	"testing"
	"time"
)

func TestSubscriptions(t *testing.T) {
	for _, types := range [][]string{{"order.created"}, {"*"}, {"a", "b"}} {
		if e := Validate("https://hooks.example.com", types); e != nil {
			t.Fatal(e)
		}
	}
	for _, types := range [][]string{nil, {""}, {"a", "a"}, {"*", "a"}, {"with space"}, {"!"}} {
		if Validate("https://hooks.example.com", types) == nil {
			t.Fatal("invalid subscriptions accepted")
		}
	}
}
func TestCursorValidation(t *testing.T) {
	tenant := security.ID()
	e := Endpoint{ID: security.ID(), CreatedAt: time.Date(2026, 10, 15, 0, 0, 0, 123000, time.UTC)}
	raw := EncodeCursor(tenant, e)
	c, err := DecodeCursor(raw, tenant)
	if err != nil || c.ID != e.ID || !c.Time.Equal(e.CreatedAt) {
		t.Fatal("round trip")
	}
	if _, err = DecodeCursor(raw, security.ID()); err == nil {
		t.Fatal("cross-tenant cursor")
	}
	for _, s := range []string{"", "invalid", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)), base64.RawURLEncoding.EncodeToString([]byte(`null`)), raw + "="} {
		if _, err = DecodeCursor(s, tenant); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	b, _ := base64.RawURLEncoding.DecodeString(raw)
	raw = base64.RawURLEncoding.EncodeToString(append(b, []byte(" junk")...))
	if _, err = DecodeCursor(raw, tenant); err == nil {
		t.Fatal("trailing garbage accepted")
	}
}
