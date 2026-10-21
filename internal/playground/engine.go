// Package playground is a bounded, ephemeral portfolio sandbox. It deliberately
// does not claim the durable PostgreSQL backend's acceptance/recovery guarantees.
package playground

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/events"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"strconv"
	"strings"
	"sync"
	"time"
)

const TTL = 30 * time.Minute
const BodyLimit = 16 << 10
const EventLimit = 10
const VisitorLimit = 128

type receipt struct {
	hash       [32]byte
	acceptance events.Acceptance
}
type event struct {
	meta events.Event
	body []byte
}
type attempt struct {
	Cycle   int       `json:"cycle_number"`
	Number  int       `json:"attempt_number"`
	Outcome string    `json:"outcome"`
	Status  int       `json:"http_status"`
	Latency int64     `json:"latency_ms"`
	Start   time.Time `json:"started_at"`
	End     time.Time `json:"finished_at"`
}
type workspace struct {
	expires    time.Time
	secret     []byte
	endpoint   endpoints.Endpoint
	events     map[string]event
	deliveries map[string]*delivery.Delivery
	attempts   map[string][]attempt
	receipts   map[string]receipt
}
type job struct{ key, id string }
type Engine struct {
	mu         sync.Mutex
	workspaces map[string]*workspace
	jobs       chan job
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	server     *http.Server
	client     *http.Client
	receiver   string
}

func New() (*Engine, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{workspaces: map[string]*workspace{}, jobs: make(chan job, 128), cancel: cancel, receiver: "http://" + listener.Addr().String() + "/webhooks", client: &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 2, IdleConnTimeout: time.Minute}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	e.server = &http.Server{Handler: http.HandlerFunc(e.receive), ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, MaxHeaderBytes: 8 << 10}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); _ = e.server.Serve(listener) }()
	for range 2 {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-e.jobs:
					e.dispatch(ctx, j)
				}
			}
		}()
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.mu.Lock()
				e.sweep()
				e.mu.Unlock()
			}
		}
	}()
	return e, nil
}
func (e *Engine) Close() {
	e.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = e.server.Shutdown(ctx)
	e.wg.Wait()
	e.client.CloseIdleConnections()
	e.mu.Lock()
	clear(e.workspaces)
	e.mu.Unlock()
}
func (e *Engine) sweep() {
	for k, w := range e.workspaces {
		if !time.Now().Before(w.expires) {
			delete(e.workspaces, k)
		}
	}
}
func (e *Engine) Create() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweep()
	if len(e.workspaces) >= VisitorLimit {
		return "", fmt.Errorf("playground full")
	}
	key, _, err := security.Token()
	if err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	e.workspaces[key] = &workspace{expires: now.Add(TTL), secret: secret, endpoint: endpoints.Endpoint{ID: security.ID(), URL: e.receiver, Enabled: true, Subscriptions: []string{"*"}, CreatedAt: now, UpdatedAt: now}, events: map[string]event{}, deliveries: map[string]*delivery.Delivery{}, attempts: map[string][]attempt{}, receipts: map[string]receipt{}}
	return key, nil
}
func (e *Engine) Remove(key string) { e.mu.Lock(); delete(e.workspaces, key); e.mu.Unlock() }
func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, msg string) {
	send(w, status, map[string]string{"message": msg})
}
func (e *Engine) ServeAPI(out http.ResponseWriter, r *http.Request, key string) {
	// Read bounded request bodies before locking shared visitor state.
	var raw []byte
	if r.Method == "POST" && r.URL.Path == "/api/v1/events" {
		var err error
		raw, err = io.ReadAll(http.MaxBytesReader(out, r.Body, BodyLimit))
		if err != nil {
			fail(out, 413, "Demo events are limited to 16 KiB.")
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweep()
	w := e.workspaces[key]
	if w == nil {
		fail(out, 401, "Demo expired. Start a new demo.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if r.Method == "POST" && path == "events" {
		input, err := events.Parse(raw)
		if err != nil {
			fail(out, 400, err.Error())
			return
		}
		idempotency := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || events.ValidateKey(idempotency) != nil {
			fail(out, 400, "Supply one idempotency key.")
			return
		}
		hash := sha256.Sum256(raw)
		if old, ok := w.receipts[idempotency]; ok {
			if old.hash != hash {
				fail(out, 409, "Idempotency key already used with different data.")
				return
			}
			out.Header().Set("Idempotency-Replayed", "true")
			send(out, 202, old.acceptance)
			return
		}
		if len(w.events) >= EventLimit {
			fail(out, 429, "This demo allows 10 events. End it and start a fresh demo.")
			return
		}
		now := time.Now().UTC()
		id, did := security.ID(), security.ID()
		body := events.Envelope(id, now, input)
		bodyhash := sha256.Sum256(body)
		select {
		case e.jobs <- job{key, did}:
		default:
			fail(out, 429, "Demo is busy. Retry with the same key.")
			return
		}
		w.events[id] = event{events.Event{ID: id, Type: input.Type, CreatedAt: now, ExpiresAt: w.expires, BodySHA256: hex.EncodeToString(bodyhash[:])}, body}
		w.deliveries[did] = &delivery.Delivery{ID: did, EventID: id, EndpointID: w.endpoint.ID, URLSnapshot: e.receiver, Status: "pending", CycleNumber: 1, CreatedAt: now, UpdatedAt: now, NextAttemptAt: now, CycleStartedAt: now}
		accepted := events.Acceptance{EventID: id, DeliveryCount: 1, AcceptedAt: now}
		w.receipts[idempotency] = receipt{hash, accepted}
		out.Header().Set("Idempotency-Replayed", "false")
		send(out, 202, accepted)
		return
	}
	if r.Method != "GET" {
		fail(out, 403, "Demo destinations are fixed. Only sample event publishing is available.")
		return
	}
	if path == "endpoints" {
		send(out, 200, map[string]any{"data": []endpoints.Endpoint{w.endpoint}, "next_cursor": ""})
		return
	}
	if path == "events" {
		list := []events.Event{}
		for _, v := range w.events {
			list = append(list, v.meta)
		}
		sortEvents(list)
		send(out, 200, map[string]any{"data": list, "next_cursor": ""})
		return
	}
	if path == "deliveries" {
		list := []delivery.Delivery{}
		for _, v := range w.deliveries {
			q := r.URL.Query()
			if s := q.Get("status"); s != "" && s != v.Status {
				continue
			}
			if s := q.Get("event_id"); s != "" && s != v.EventID {
				continue
			}
			if s := q.Get("endpoint_id"); s != "" && s != v.EndpointID {
				continue
			}
			list = append(list, *v)
		}
		sortDeliveries(list)
		send(out, 200, map[string]any{"data": list, "next_cursor": ""})
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] == "events" {
		v, ok := w.events[parts[1]]
		if !ok {
			fail(out, 404, "Not found.")
			return
		}
		counts := map[string]int{}
		for _, d := range w.deliveries {
			if d.EventID == parts[1] {
				counts[d.Status]++
			}
		}
		send(out, 200, events.Detail{Event: v.meta, Payload: json.RawMessage(v.body), DeliverySummary: counts})
		return
	}
	if len(parts) >= 2 && parts[0] == "deliveries" {
		v := w.deliveries[parts[1]]
		if v == nil {
			fail(out, 404, "Not found.")
			return
		}
		if len(parts) == 2 {
			send(out, 200, v)
			return
		}
		if len(parts) == 3 && parts[2] == "attempts" {
			a := w.attempts[v.ID]
			if a == nil {
				a = []attempt{}
			}
			send(out, 200, map[string]any{"data": a, "next_cursor": ""})
			return
		}
		if len(parts) == 3 && parts[2] == "replays" {
			send(out, 200, map[string]any{"data": []any{}, "next_cursor": ""})
			return
		}
	}
	fail(out, 404, "Demo route unavailable.")
}
func (e *Engine) receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/webhooks" {
		w.WriteHeader(404)
		return
	}
	key := r.Header.Get("X-Demo-Workspace")
	id := r.Header.Get("webhook-id")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, BodyLimit+1024))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	e.mu.Lock()
	workspace := e.workspaces[key]
	if workspace == nil || !time.Now().Before(workspace.expires) || workspace.deliveries[id] == nil {
		e.mu.Unlock()
		w.WriteHeader(401)
		return
	}
	secret := append([]byte{}, workspace.secret...)
	d := workspace.deliveries[id]
	kind := workspace.events[d.EventID].meta.Type
	attemptCount := d.CycleAttemptCount
	e.mu.Unlock()
	if signing.Verify(id, r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), raw, [][]byte{secret}, time.Now(), 5*time.Minute) != nil {
		w.WriteHeader(401)
		return
	}
	if kind == "demo.retry" && attemptCount == 1 {
		w.WriteHeader(503)
		return
	}
	w.WriteHeader(204)
}
func (e *Engine) dispatch(ctx context.Context, j job) {
	for number := 1; number <= 2; number++ {
		e.mu.Lock()
		w := e.workspaces[j.key]
		if w == nil || !time.Now().Before(w.expires) {
			e.mu.Unlock()
			return
		}
		d := w.deliveries[j.id]
		d.Status = "in_flight"
		d.CycleAttemptCount = number
		d.LifetimeAttemptCount = number
		d.UpdatedAt = time.Now().UTC()
		body := append([]byte{}, w.events[d.EventID].body...)
		secret := append([]byte{}, w.secret...)
		e.mu.Unlock()
		start := time.Now().UTC()
		stamp := strconv.FormatInt(start.Unix(), 10)
		signature, _ := signing.Sign(j.id, stamp, body, [][]byte{secret})
		req, _ := http.NewRequestWithContext(ctx, "POST", e.receiver, bytes.NewReader(body))
		req.Header.Set("X-Demo-Workspace", j.key)
		req.Header.Set("webhook-id", j.id)
		req.Header.Set("webhook-timestamp", stamp)
		req.Header.Set("webhook-signature", signature)
		req.Header.Set("Content-Type", "application/json")
		response, err := e.client.Do(req)
		status := 0
		if err == nil {
			status = response.StatusCode
			response.Body.Close()
		}
		end := time.Now().UTC()
		success := status == 204
		outcome := "retryable_failure"
		if success {
			outcome = "success"
		}
		e.mu.Lock()
		w = e.workspaces[j.key]
		if w == nil {
			e.mu.Unlock()
			return
		}
		d = w.deliveries[j.id]
		w.attempts[j.id] = append(w.attempts[j.id], attempt{1, number, outcome, status, end.Sub(start).Milliseconds(), start, end})
		d.LastHTTPStatus = &status
		d.UpdatedAt = end
		if success {
			d.Status = "delivered"
			d.DeliveredAt = &end
			e.mu.Unlock()
			return
		}
		d.Status = "retry_scheduled"
		d.NextAttemptAt = end.Add(time.Second)
		if number == 2 {
			d.Status = "dead_letter"
		}
		e.mu.Unlock()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
