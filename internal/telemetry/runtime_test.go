package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsBoundedLabelsAndUntrustedTraceHeadersIgnored(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	r, e := New(context.Background(), Config{Role: "api", SampleRatio: 1, Exporter: exporter, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Shutdown(context.Background())
	handler := r.HTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Idempotency-Replayed", "false")
		w.WriteHeader(202)
	}))
	req := httptest.NewRequest("POST", "/v1/events", nil)
	req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	req.Header.Set("baggage", "private=must-not-export")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Header().Get("X-Trace-ID") == "11111111111111111111111111111111" {
		t.Fatal("public trace context trusted")
	}
	if e = r.Traces.ForceFlush(context.Background()); e != nil {
		t.Fatal(e)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Parent.IsValid() || len(spans[0].Links) != 0 {
		t.Fatal("incoming parent/baggage accepted")
	}
	for _, a := range spans[0].Attributes {
		if strings.Contains(a.Value.AsString(), "must-not-export") {
			t.Fatal("baggage leaked")
		}
	}
	metrics := httptest.NewRecorder()
	r.Handler.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `relayforge_events_total{result="accepted",role="api"} 1`) {
		t.Fatal(metrics.Body.String())
	}
	for _, raw := range []string{"/v1/events/secret-event-id", "/v1/deliveries/secret-delivery-id/replay", "/v1/endpoints/secret-endpoint-id"} {
		if strings.Contains(Route(raw), "secret") {
			t.Fatal("unbounded route")
		}
	}
}

type brokenSource struct{}

func (brokenSource) Observe(context.Context) (Snapshot, error) {
	return Snapshot{}, errors.New("postgres://private:secret@host")
}
func TestUnavailableSnapshotDoesNotReportEmptyQueue(t *testing.T) {
	r, e := New(context.Background(), Config{Role: "api"})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Shutdown(context.Background())
	if e = r.Observe(brokenSource{}, nil); e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	raw := w.Body.String()
	if !strings.Contains(raw, `relayforge_queue_snapshot_up{role="api"} 0`) || strings.Contains(raw, "private") || strings.Contains(raw, "relayforge_queue_deliveries{") {
		t.Fatal("unavailable queue looked empty", raw)
	}
}
func TestTraceExporterErrorsAreSanitizedAndNoncritical(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "collector-private-error", 503) }))
	defer collector.Close()
	runtime, e := New(context.Background(), Config{Role: "api", TraceEndpoint: collector.URL + "/v1/traces", SampleRatio: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Shutdown(context.Background())
	if e = runtime.Observe(nil, nil); e != nil {
		t.Fatal(e)
	}
	_, span := runtime.Start(context.Background(), "safe.operation")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = runtime.Traces.ForceFlush(ctx)
	handler := runtime.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/events", nil))
	if w.Code != 202 {
		t.Fatal("exporter failure affected acceptance")
	}
	metric := httptest.NewRecorder()
	runtime.Handler.ServeHTTP(metric, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metric.Body.String(), `relayforge_telemetry_export_errors_total{role="api",signal="traces"} 1`) || strings.Contains(metric.Body.String(), "collector-private-error") {
		t.Fatal(metric.Body.String())
	}
}

func TestRequestIDCorrelatesEntityLogsWithoutTracing(t *testing.T) {
	var output bytes.Buffer
	r, e := New(context.Background(), Config{Role: "api", Log: slog.New(slog.NewJSONHandler(&output, nil))})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Shutdown(context.Background())
	ctx := WithRequestID(context.Background(), "server-generated-request-id")
	r.EntityLog(ctx, "event acceptance committed", "event_id", "event-reference")
	var log map[string]any
	if e = json.Unmarshal(output.Bytes(), &log); e != nil {
		t.Fatal(e)
	}
	if log["request_id"] != "server-generated-request-id" || log["event_id"] != "event-reference" {
		t.Fatal("request/object correlation lost")
	}
	if _, exists := log["trace_id"]; exists {
		t.Fatal("tracing was disabled")
	}
}
