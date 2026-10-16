package httpapi

import (
	"net/http/httptest"
	"relayforge/internal/endpoints"
	"strings"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	for _, raw := range []string{`{}`, `{"a":[{"b":1}]}`} {
		if e := validateJSON([]byte(raw)); e != nil {
			t.Fatal(e)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"a":1,"a":2}`, `{"a":{"x":1,"x":2}}`, `{} {}`, `{}junk`, `{"a":}`, `{"a":1,}`, "{\"a\":\"\xff\"}", `{"a":` + strings.Repeat(`[`, 66) + `0` + strings.Repeat(`]`, 66) + `}`} {
		if validateJSON([]byte(raw)) == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}
func TestDecodeRejectsMalformedEndpointBodies(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"url":"x","URL":"y"}`, 400}, {`{"enabled":null}`, 400}, {`{"tenant_id":"other"}`, 400}, {`{"enabled":false,"enabled":true}`, 400}, {`{"url":"` + strings.Repeat("x", 256<<10) + `"}`, 413}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		var input endpoints.Patch
		if decode(w, r, &input) || w.Code != tc.status {
			t.Fatalf("expected %d, got %d", tc.status, w.Code)
		}
	}
}
