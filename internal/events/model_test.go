package events

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseAndImmutableEnvelope(t *testing.T) {
	raw := []byte(` {"type":"order.created", "data": { "amount": 12500, "nested": [true,null] }} `)
	input, e := Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 123000, time.FixedZone("offset", 19800))
	body := Envelope("event-id", now, input)
	if !json.Valid(body) || !bytes.Contains(body, input.Data) || !bytes.Contains(body, []byte(`"created_at":"2026-10-15T06:30:00.000123Z"`)) {
		t.Fatal("envelope did not preserve validated data/UTC timestamp")
	}
	again := Envelope("event-id", now, input)
	if !bytes.Equal(body, again) {
		t.Fatal("nondeterministic envelope")
	}
	if bytes.Equal(Hash(raw), Hash(bytes.TrimSpace(raw))) {
		t.Fatal("exact-input whitespace was lost")
	}
	for _, data := range []string{`null`, `[1,2]`, `"text"`, `42`, `true`, `{}`} {
		if _, e = Parse([]byte(`{"type":"a","data":` + data + `}`)); e != nil {
			t.Fatal("valid JSON data rejected")
		}
	}
}
func TestInvalidEventAndKey(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"type":"a"}`, `{"data":{}}`, `{"type":"*","data":{}}`, `{"type":"bad type","data":{}}`, `{"type":null,"data":{}}`, `{"Type":"a","data":{}}`, `{"type":"a","data":{},"tenant_id":"spoof"}`, `{"type":"a","type":"a","data":{}}`, `{"type":"a","data":{"n":1,"n":2}}`, `{"type":"a","data":{}} {}`, `{"type":"a","data":"` + strings.Repeat("x", MaxInputBytes) + `"}`} {
		if _, e := Parse([]byte(raw)); e == nil {
			t.Fatal("invalid input accepted")
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 129), "tab\tkey", "line\nkey", "ключ", string([]byte{127})} {
		if ValidateKey(key) == nil {
			t.Fatal("invalid idempotency key accepted")
		}
	}
	for _, key := range []string{"a", "with internal spaces", strings.Repeat("x", 128)} {
		if e := ValidateKey(key); e != nil {
			t.Fatal(e)
		}
	}
}
