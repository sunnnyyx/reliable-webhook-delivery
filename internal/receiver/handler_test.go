package receiver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"strconv"
	"testing"
	"time"
)

func TestReceiverAcknowledgesValidRawBodyAndRejectsTampering(t *testing.T) {
	now := time.Unix(1700000000, 0)
	key := bytes.Repeat([]byte{4}, 32)
	id := security.ID()
	raw := []byte(`{"id":"` + security.ID() + `","type":"order.created","created_at":"2026-10-15T00:00:00Z","data":{"n":1}}`)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	sig, e := signing.Sign(id, timestamp, raw, [][]byte{key})
	if e != nil {
		t.Fatal(e)
	}
	handler := Handler([][]byte{key}, slog.New(slog.NewJSONHandler(io.Discard, nil)), func() time.Time { return now }, processFunc(func(context.Context, string, Envelope, []byte) (bool, error) { return false, nil }))
	for _, tc := range []struct {
		body   []byte
		stamp  string
		status int
	}{{raw, timestamp, 204}, {append(append([]byte{}, raw...), byte(' ')), timestamp, 401}, {raw, strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10), 401}} {
		r := httptest.NewRequest("POST", "/webhooks", bytes.NewReader(tc.body))
		r.Header.Set("webhook-id", id)
		r.Header.Set("webhook-timestamp", tc.stamp)
		r.Header.Set("webhook-signature", sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("receiver status %d want %d", w.Code, tc.status)
		}
	}
}

type processFunc func(context.Context, string, Envelope, []byte) (bool, error)

func (f processFunc) Process(ctx context.Context, id string, e Envelope, b []byte) (bool, error) {
	return f(ctx, id, e, b)
}

func TestReceiverRequiresStorageAndStrictAuthenticatedEnvelope(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	id := security.ID()
	now := time.Now()
	stamp := strconv.FormatInt(now.Unix(), 10)
	valid := []byte(`{"id":"` + security.ID() + `","type":"order.created","created_at":"2026-10-15T00:00:00Z","data":{}}`)
	calls := 0
	processor := processFunc(func(context.Context, string, Envelope, []byte) (bool, error) { calls++; return false, nil })
	for _, tc := range []struct {
		raw   []byte
		store Processor
		want  int
	}{
		{valid, nil, 503},
		{bytes.Replace(valid, []byte(`"id":`), []byte(`"ID":`), 1), processor, 400},
		{append(append([]byte{}, valid[:len(valid)-1]...), []byte(`,"extra":true}`)...), processor, 400},
		{[]byte(`{"id":"` + id + `","id":"` + id + `","type":"order.created","created_at":"2026-10-15T00:00:00Z","data":{}}`), processor, 400},
	} {
		sig, e := signing.Sign(id, stamp, tc.raw, [][]byte{key})
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest("POST", "/webhooks", bytes.NewReader(tc.raw))
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", stamp)
		req.Header.Set("webhook-signature", sig)
		recorder := httptest.NewRecorder()
		Handler([][]byte{key}, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now }, tc.store).ServeHTTP(recorder, req)
		if recorder.Code != tc.want {
			t.Fatal(recorder.Code, tc.want)
		}
	}
	if calls != 0 {
		t.Fatal("invalid requests touched storage")
	}
}
