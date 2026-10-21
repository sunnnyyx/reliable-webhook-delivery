package playground

import (
	"encoding/json"
	"net/http/httptest"
	"relayforge/internal/delivery"
	"relayforge/internal/events"
	"strings"
	"testing"
	"time"
)

func request(e *Engine, key, method, path, body, idempotency string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://demo/api/v1/"+path, strings.NewReader(body))
	if idempotency != "" {
		r.Header.Set("Idempotency-Key", idempotency)
	}
	out := httptest.NewRecorder()
	e.ServeAPI(out, r, key)
	return out
}
func TestRealSignedDeliveryRetryAndIsolation(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a, _ := e.Create()
	b, _ := e.Create()
	out := request(e, a, "POST", "events", `{"type":"demo.retry","data":{"example":true}}`, "same-key")
	if out.Code != 202 {
		t.Fatal(out.Body.String())
	}
	var accepted events.Acceptance
	_ = json.Unmarshal(out.Body.Bytes(), &accepted)
	duplicate := request(e, a, "POST", "events", `{"type":"demo.retry","data":{"example":true}}`, "same-key")
	if duplicate.Code != 202 || duplicate.Header().Get("Idempotency-Replayed") != "true" || duplicate.Body.String() != out.Body.String() {
		t.Fatal("receipt changed")
	}
	if request(e, a, "POST", "events", `{"type":"other","data":{}}`, "same-key").Code != 409 {
		t.Fatal("idempotency conflict lost")
	}
	deadline := time.Now().Add(5 * time.Second)
	var d delivery.Delivery
	for time.Now().Before(deadline) {
		list := request(e, a, "GET", "deliveries", "", "")
		var page struct {
			Data []delivery.Delivery `json:"data"`
		}
		_ = json.Unmarshal(list.Body.Bytes(), &page)
		if len(page.Data) > 0 {
			d = page.Data[0]
			if d.Status == "delivered" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.Status != "delivered" || d.LifetimeAttemptCount != 2 {
		t.Fatalf("retry result %#v", d)
	}
	history := request(e, a, "GET", "deliveries/"+d.ID+"/attempts", "", "")
	var attempts struct {
		Data []attempt `json:"data"`
	}
	_ = json.Unmarshal(history.Body.Bytes(), &attempts)
	if len(attempts.Data) != 2 || attempts.Data[0].Status != 503 || attempts.Data[1].Status != 204 {
		t.Fatal("real attempt history missing")
	}
	if request(e, b, "GET", "events/"+accepted.EventID, "", "").Code != 404 || request(e, b, "GET", "deliveries/"+d.ID, "", "").Code != 404 {
		t.Fatal("visitor data leaked")
	}
	if request(e, a, "POST", "endpoints", `{"url":"https://evil.example"}`, "").Code != 403 {
		t.Fatal("external destinations enabled")
	}
	if request(e, a, "POST", "deliveries/"+d.ID+"/replay", `{}`, "key").Code != 403 {
		t.Fatal("unbounded replay enabled")
	}
}
func TestLimitsExpiryAndUnsignedReceiver(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	key, _ := e.Create()
	bad := httptest.NewRecorder()
	e.receive(bad, httptest.NewRequest("POST", "http://demo/webhooks", strings.NewReader(`{}`)))
	if bad.Code != 401 {
		t.Fatal("unsigned request accepted")
	}
	for i := 0; i < EventLimit; i++ {
		out := request(e, key, "POST", "events", `{"type":"sample","data":{}}`, string(rune('a'+i)))
		if out.Code != 202 {
			t.Fatal("quota too low")
		}
	}
	if request(e, key, "POST", "events", `{"type":"sample","data":{}}`, "overflow").Code != 429 {
		t.Fatal("event quota ignored")
	}
	if request(e, key, "POST", "events", strings.Repeat("x", BodyLimit+1), "large").Code != 413 {
		t.Fatal("body size not bounded")
	}
	e.mu.Lock()
	e.workspaces[key].expires = time.Now().Add(-time.Second)
	e.mu.Unlock()
	if request(e, key, "GET", "events", "", "").Code != 401 {
		t.Fatal("expired data readable")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.workspaces) != 0 {
		t.Fatal("expired workspace retained")
	}
}
