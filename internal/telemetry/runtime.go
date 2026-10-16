package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	exporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	metricSDK "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	traceSDK "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

type Config struct {
	Role          string
	TraceEndpoint string
	SampleRatio   float64
	Log           *slog.Logger
	// Exporter injection is for in-process SDK tests; production uses TraceEndpoint.
	Exporter traceSDK.SpanExporter
}
type Runtime struct {
	Role         string
	Log          *slog.Logger
	Metrics      *metricSDK.MeterProvider
	Traces       *traceSDK.TracerProvider
	Tracer       trace.Tracer
	Handler      http.Handler
	counters     map[string]metric.Int64Counter
	histograms   map[string]metric.Float64Histogram
	exporterUp   atomic.Bool
	traceEnabled bool
}

func New(ctx context.Context, c Config) (*Runtime, error) {
	if c.Role != "api" && c.Role != "worker" || c.SampleRatio < 0 || c.SampleRatio > 1 {
		return nil, errors.New("invalid telemetry configuration")
	}
	r := &Runtime{Role: c.Role, Log: c.Log, Tracer: trace.NewNoopTracerProvider().Tracer("relayforge"), counters: map[string]metric.Int64Counter{}, histograms: map[string]metric.Float64Histogram{}}
	registry := prometheus.NewRegistry()
	e, err := exporter.New(exporter.WithRegisterer(prometheus.WrapRegistererWith(prometheus.Labels{"role": c.Role}, registry)), exporter.WithoutTargetInfo(), exporter.WithoutScopeInfo(), exporter.WithoutUnits(), exporter.WithoutCounterSuffixes())
	if err != nil {
		return nil, errors.New("metrics initialization failed")
	}
	r.Metrics = metricSDK.NewMeterProvider(metricSDK.WithReader(e))
	meter := r.Metrics.Meter("relayforge")
	for _, name := range []string{"api_requests", "events", "dispatch_attempts", "completions", "replays", "recoveries", "rate_limit_deferrals", "rate_limit_permissions", "rate_limit_denials", "rate_limit_errors", "telemetry_export_errors"} {
		v, e := meter.Int64Counter("relayforge_" + name + "_total")
		if e != nil {
			return nil, e
		}
		r.counters[name] = v
	}
	for _, name := range []string{"acceptance_duration", "dispatch_duration", "first_attempt_queue_delay", "event_to_ack_duration"} {
		v, e := meter.Float64Histogram("relayforge_"+name+"_seconds", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300, 3600, 86400))
		if e != nil {
			return nil, e
		}
		r.histograms[name] = v
	}
	r.Handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: false})
	delegate := c.Exporter
	if delegate == nil && c.TraceEndpoint != "" {
		client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second, MaxResponseHeaderBytes: 16 << 10}}
		delegate, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.TraceEndpoint), otlptracehttp.WithHeaders(map[string]string{}), otlptracehttp.WithTimeout(2*time.Second), otlptracehttp.WithMaxResponseSize(16<<10), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}), otlptracehttp.WithHTTPClient(client))
		if err != nil {
			r.Metrics.Shutdown(ctx)
			return nil, errors.New("trace initialization failed")
		}
	}
	if delegate != nil {
		r.traceEnabled = true
		r.exporterUp.Store(true)
		r.Traces = traceSDK.NewTracerProvider(traceSDK.WithResource(resource.NewSchemaless(attribute.String("service.name", "relayforge-"+c.Role))), traceSDK.WithSampler(traceSDK.ParentBased(traceSDK.TraceIDRatioBased(c.SampleRatio))), traceSDK.WithBatcher(&safeExporter{delegate: delegate, r: r}, traceSDK.WithMaxQueueSize(512), traceSDK.WithMaxExportBatchSize(128), traceSDK.WithBatchTimeout(time.Second), traceSDK.WithExportTimeout(2*time.Second)))
		r.Tracer = r.Traces.Tracer("relayforge")
	}
	return r, nil
}

type safeExporter struct {
	delegate traceSDK.SpanExporter
	r        *Runtime
}

func (s *safeExporter) ExportSpans(ctx context.Context, spans []traceSDK.ReadOnlySpan) error {
	if e := s.delegate.ExportSpans(ctx, spans); e != nil {
		s.r.exporterUp.Store(false)
		s.r.Count(context.Background(), "telemetry_export_errors", "signal", "traces")
		if s.r.Log != nil {
			s.r.Log.Warn("trace export failed", "error_code", "telemetry_unavailable")
		}
		return errors.New("trace export unavailable")
	}
	s.r.exporterUp.Store(true)
	return nil
}
func (s *safeExporter) Shutdown(ctx context.Context) error {
	if e := s.delegate.Shutdown(ctx); e != nil {
		return errors.New("trace shutdown unavailable")
	}
	return nil
}
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var err error
	if r.Traces != nil {
		err = r.Traces.Shutdown(ctx)
	}
	return errors.Join(err, r.Metrics.Shutdown(ctx))
}
func (r *Runtime) Count(ctx context.Context, name, key, value string) {
	if r != nil {
		if c := r.counters[name]; c != nil {
			if key == "" {
				c.Add(ctx, 1)
			} else {
				c.Add(ctx, 1, metric.WithAttributes(attribute.String(key, value)))
			}
		}
	}
}
func (r *Runtime) Duration(ctx context.Context, name string, d time.Duration) {
	if r != nil {
		if h := r.histograms[name]; h != nil {
			if d < 0 {
				d = 0
			}
			h.Record(ctx, d.Seconds())
		}
	}
}
func (r *Runtime) Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if r == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	return r.Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
func Finish(span trace.Span, category string) {
	if category != "" {
		span.SetStatus(codes.Error, category)
		span.SetAttributes(attribute.String("error_code", category))
	}
	span.End()
}
func (r *Runtime) EntityLog(ctx context.Context, message string, fields ...any) {
	if r == nil || r.Log == nil {
		return
	}
	if id := RequestID(ctx); id != "" {
		fields = append(fields, "request_id", id)
	}
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		fields = append(fields, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	r.Log.Info(message, fields...)
}
