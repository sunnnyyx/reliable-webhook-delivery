//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/httpapi"
	"relayforge/internal/netpolicy"
	"relayforge/internal/ratelimit"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"relayforge/internal/telemetry"
	"relayforge/internal/worker"
	"strconv"
	"strings"
	"testing"
	"time"
)

func metricText(t *testing.T, r *telemetry.Runtime) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatal("metrics unavailable")
	}
	return w.Body.String()
}
func metricValue(raw, name string, labels ...string) (float64, bool) {
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		match := true
		for _, label := range labels {
			if !strings.Contains(line, label) {
				match = false
			}
		}
		if match {
			parts := strings.Fields(line)
			v, e := strconv.ParseFloat(parts[len(parts)-1], 64)
			return v, e == nil
		}
	}
	return 0, false
}
func wantMetric(t *testing.T, raw, name string, value float64, labels ...string) {
	t.Helper()
	v, ok := metricValue(raw, name, labels...)
	if !ok || v != value {
		t.Fatalf("metric %s=%g found=%v want=%g", name, v, ok, value)
	}
}
func TestTelemetryAcceptanceDispatchReplayAndTraceLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Telemetry")
	apiExport, workerExport := tracetest.NewInMemoryExporter(), tracetest.NewInMemoryExporter()
	apiRuntime, e := telemetry.New(ctx, telemetry.Config{Role: "api", Exporter: apiExport, SampleRatio: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	defer apiRuntime.Shutdown(ctx)
	workerRuntime, e := telemetry.New(ctx, telemetry.Config{Role: "worker", Exporter: workerExport, SampleRatio: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer workerRuntime.Shutdown(ctx)
	s.Telemetry = apiRuntime
	if e = apiRuntime.Observe(s, map[string]telemetry.Pinger{"postgres": s.DB}); e != nil {
		t.Fatal(e)
	}
	requestBodies := make(chan []byte, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" || r.Header.Get("tracestate") != "" || r.Header.Get("baggage") != "" || r.Header.Get("Authorization") != "" {
			t.Error("internal context leaked to receiver")
		}
		body, _ := io.ReadAll(r.Body)
		requestBodies <- body
		if len(requestBodies) == 1 {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer receiver.Close()
	u, _ := url.Parse(receiver.URL)
	destination := "http://localhost:" + u.Port() + "/webhooks"
	policy, e := netpolicy.New("development", destination, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	_, secret, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	raw := []byte(`{"type":"order.created","data":{"marker":"telemetry-private-payload"}}`)
	post := func(path, key, token string, body []byte, want int) ([]byte, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
		req.Header.Set("baggage", "secret=telemetry-private-baggage")
		response, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		b, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("HTTP %d want %d", response.StatusCode, want)
		}
		return b, response.Header
	}
	accepted, headers := post("/v1/events", "telemetry-event", c.APIKey, raw, 202)
	var event struct {
		ID string `json:"event_id"`
	}
	if e = json.Unmarshal(accepted, &event); e != nil {
		t.Fatal(e)
	}
	detail, e := s.GetEvent(ctx, c.TenantID, event.ID)
	if e != nil || detail.TraceID == "" || detail.TraceID != headers.Get("X-Trace-ID") || detail.TraceID == "11111111111111111111111111111111" {
		t.Fatal("stored acceptance trace incorrect", e)
	}
	post("/v1/events", "telemetry-event", c.APIKey, raw, 202)
	post("/v1/events", "bad-json", c.APIKey, []byte(`{`), 400)
	post("/v1/events", "bad-auth", "invalid", raw, 401)
	initial := metricText(t, apiRuntime)
	wantMetric(t, initial, "relayforge_queue_deliveries", 1, `status="pending"`)
	repository := &postgres.Store{DB: s.DB, Vault: s.Vault, Policy: policy, Telemetry: workerRuntime}
	client := worker.NewClient(policy, time.Second, 1)
	defer client.CloseIdleConnections()
	captured := make(chan delivery.Job, 2)
	run := func(want string) {
		t.Helper()
		runctx, cancel := context.WithCancel(ctx)
		sender := &worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
		runner := worker.Runner{Repository: repository, Sender: captureDispatch{sender, captured}, Telemetry: workerRuntime, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ID: security.ID(), Concurrency: 1, Poll: 10 * time.Millisecond, Lease: time.Minute, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
		done := make(chan struct{})
		go func() { runner.Run(runctx); close(done) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var status string
			if e = s.DB.QueryRow(ctx, `SELECT status FROM deliveries`).Scan(&status); e != nil {
				t.Fatal(e)
			}
			if status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("delivery transition timeout")
	}
	run("dead_letter")
	dead := metricText(t, apiRuntime)
	wantMetric(t, dead, "relayforge_queue_deliveries", 1, `status="dead_letter"`)
	var id string
	if e = s.DB.QueryRow(ctx, `SELECT id FROM deliveries`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	path := "/v1/deliveries/" + id + "/replay"
	post(path, "repair", c.APIKey, []byte(`{}`), 202)
	post(path, "repair", c.APIKey, []byte(`{}`), 202)
	run("delivered")
	old := <-captured
	newer := <-captured
	if old.ID != newer.ID || !bytes.Equal(old.Body, newer.Body) || !bytes.Equal(<-requestBodies, <-requestBodies) {
		t.Fatal("tracing changed stored bytes")
	}
	if ok, e := repository.Complete(ctx, old, delivery.Classify(204)); e != nil || ok {
		t.Fatal("stale completion accepted")
	}
	apiMetrics, workerMetrics := metricText(t, apiRuntime), metricText(t, workerRuntime)
	for _, result := range []string{"accepted", "idempotent", "invalid_input", "unauthorized"} {
		wantMetric(t, apiMetrics, "relayforge_events_total", 1, `result="`+result+`"`)
	}
	wantMetric(t, apiMetrics, "relayforge_queue_deliveries", 1, `status="delivered"`)
	wantMetric(t, apiMetrics, "relayforge_replays_total", 1, `result="accepted"`)
	wantMetric(t, workerMetrics, "relayforge_dispatch_attempts_total", 1, `outcome="permanent_failure"`, `status_class="4xx"`)
	wantMetric(t, workerMetrics, "relayforge_dispatch_attempts_total", 1, `outcome="success"`, `status_class="2xx"`)
	wantMetric(t, workerMetrics, "relayforge_completions_total", 1, `result="stale"`)
	wantMetric(t, workerMetrics, "relayforge_first_attempt_queue_delay_seconds_count", 1)
	wantMetric(t, workerMetrics, "relayforge_event_to_ack_duration_seconds_count", 1)
	if e = apiRuntime.Traces.ForceFlush(ctx); e != nil {
		t.Fatal(e)
	}
	if e = workerRuntime.Traces.ForceFlush(ctx); e != nil {
		t.Fatal(e)
	}
	var acceptedSpan string
	for _, span := range apiExport.GetSpans() {
		if span.Name == "event.accept" && span.SpanContext.TraceID().String() == detail.TraceID {
			acceptedSpan = span.SpanContext.SpanID().String()
		}
	}
	if acceptedSpan == "" {
		t.Fatal("acceptance span not exported")
	}
	dispatches := 0
	for _, span := range workerExport.GetSpans() {
		if span.Name == "delivery.dispatch" {
			dispatches++
			if span.Parent.IsValid() || len(span.Links) != 1 || span.Links[0].SpanContext.TraceID().String() != detail.TraceID || span.Links[0].SpanContext.SpanID().String() != acceptedSpan {
				t.Fatal("async acceptance link lost")
			}
		}
	}
	if dispatches != 2 {
		t.Fatal("dispatch spans missing", dispatches)
	}
	metadata := fmt.Sprint(apiExport.GetSpans(), workerExport.GetSpans())
	for _, private := range []string{c.APIKey, secret, "telemetry-private-payload", "telemetry-private-baggage", destination} {
		if strings.Contains(apiMetrics+workerMetrics+metadata, private) {
			t.Fatal("telemetry disclosed private content")
		}
	}
	for _, identifier := range []string{c.TenantID, event.ID, id} {
		if strings.Contains(apiMetrics+workerMetrics, identifier) {
			t.Fatal("identifier used as metric label")
		}
	}
}

type captureDispatch struct {
	sender   *worker.Sender
	captured chan delivery.Job
}

func (c captureDispatch) Send(ctx context.Context, j delivery.Job) delivery.Result {
	c.captured <- j
	return c.sender.Send(ctx, j)
}

type denyPermission struct{}

func (denyPermission) Allow(context.Context, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{Wait: time.Second}, nil
}
func TestTelemetryRecoveryDeferralAndUnavailableSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Telemetry recovery")
	targets(t, s, c.TenantID, "order.created")
	r, e := telemetry.New(ctx, telemetry.Config{Role: "worker"})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Shutdown(ctx)
	s.Telemetry = r
	if e = r.Observe(s, map[string]telemetry.Pinger{"postgres": s.DB}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Accept(ctx, c.TenantID, "deferred", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	s.Limiter = denyPermission{}
	if j, e := s.Claim(ctx, security.ID(), time.Minute); e != nil || j != nil {
		t.Fatal(e)
	}
	wantMetric(t, metricText(t, r), "relayforge_rate_limit_deferrals_total", 1, `reason="rate_limited"`)
	if _, e = s.DB.Exec(ctx, `UPDATE deliveries SET cycle_started_at=clock_timestamp()-interval '25 hours'`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Recover(ctx, 100); e != nil {
		t.Fatal(e)
	}
	wantMetric(t, metricText(t, r), "relayforge_recoveries_total", 1, `reason="cycle_expired"`)
	s.Limiter = nil
	if _, e = s.Accept(ctx, c.TenantID, "expired-lease", []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE tenant_dispatch_state SET dispatch_after=clock_timestamp()`); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Recover(ctx, 100); e != nil {
		t.Fatal(e)
	}
	wantMetric(t, metricText(t, r), "relayforge_recoveries_total", 1, `reason="lease_expired"`)
	s.DB.Close()
	raw := metricText(t, r)
	wantMetric(t, raw, "relayforge_dependency_up", 0, `dependency="postgres"`)
	wantMetric(t, raw, "relayforge_queue_snapshot_up", 0)
	if _, ok := metricValue(raw, "relayforge_queue_deliveries", `status="pending"`); ok {
		t.Fatal("unavailable storage emitted a zero/old queue snapshot")
	}
	if strings.Contains(raw, "postgres://") {
		t.Fatal("database URL in metrics")
	}
}

func TestReadinessRejectsMissingCriticalSchema(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	get := func(want int) {
		t.Helper()
		response, e := server.Client().Get(server.URL + "/health/ready")
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatal("readiness", response.StatusCode, want)
		}
		if strings.Contains(string(body), "event_trace_context") {
			t.Fatal("schema detail leaked")
		}
	}
	get(200)
	if _, e := s.DB.Exec(context.Background(), `ALTER TABLE event_trace_context RENAME TO unavailable_trace_context`); e != nil {
		t.Fatal(e)
	}
	get(503)
	if _, e := s.DB.Exec(context.Background(), `ALTER TABLE unavailable_trace_context RENAME TO event_trace_context`); e != nil {
		t.Fatal(e)
	}
	get(200)
}

func TestCollectorOutageDoesNotBlockDurableAPI(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Collector outage")
	targets(t, s, c.TenantID, "order.created")
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "collector-private-diagnostic", 503) }))
	defer collector.Close()
	observation, e := telemetry.New(ctx, telemetry.Config{Role: "api", TraceEndpoint: collector.URL + "/v1/traces", SampleRatio: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer observation.Shutdown(ctx)
	s.Telemetry = observation
	if e = observation.Observe(s, map[string]telemetry.Pinger{"postgres": s}); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(httpapi.API(s, time.Second))
	defer server.Close()
	submitEvent := func(key string) {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+"/v1/events", bytes.NewBufferString(orderBody))
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		response, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 202 || strings.Contains(string(body), "collector-private") {
			t.Fatal("collector affected acceptance")
		}
	}
	submitEvent("before-export-failure")
	_ = observation.Traces.ForceFlush(ctx)
	wantMetric(t, metricText(t, observation), "relayforge_trace_exporter_up", 0)
	submitEvent("after-export-failure")
	response, e := server.Client().Get(server.URL + "/health/ready")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("noncritical collector changed readiness")
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); e != nil || n != 2 {
		t.Fatal("acceptances were not durable", n, e)
	}
}
