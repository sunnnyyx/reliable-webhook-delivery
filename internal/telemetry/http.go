package telemetry

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"net/http"
	"strings"
	"time"
)

func Class(status int) string {
	if status >= 100 && status <= 599 {
		return string(rune('0'+status/100)) + "xx"
	}
	return "none"
}
func Route(path string) string {
	switch {
	case path == "/v1/events":
		return "events"
	case strings.HasPrefix(path, "/v1/deliveries/") && strings.HasSuffix(path, "/replay"):
		return "replay"
	case strings.HasPrefix(path, "/v1/endpoints"):
		return "endpoints"
	case strings.HasPrefix(path, "/v1/events/") || strings.HasPrefix(path, "/v1/deliveries"):
		return "history"
	case strings.HasPrefix(path, "/health/"):
		return "health"
	default:
		return "unmatched"
	}
}
func Method(method string) string {
	switch method {
	case "GET", "POST", "PATCH", "DELETE":
		return method
	default:
		return "other"
	}
}
func EventResult(status int, reused bool) string {
	switch status {
	case 202:
		if reused {
			return "idempotent"
		}
		return "accepted"
	case 400:
		return "invalid_input"
	case 401:
		return "unauthorized"
	case 404:
		return "not_found"
	case 409:
		return "idempotency_mismatch"
	case 413:
		return "payload_too_large"
	case 429:
		return "backlog_limit"
	case 503:
		return "storage_unavailable"
	default:
		return "other"
	}
}

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (r *Runtime) HTTP(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started := time.Now()
		route := Route(req.URL.Path)
		// Public incoming traceparent/tracestate/baggage are intentionally not extracted.
		ctx, span := r.Tracer.Start(req.Context(), "api."+route, trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("route", route), attribute.String("method", Method(req.Method))))
		if span.SpanContext().IsValid() {
			w.Header().Set("X-Trace-ID", span.SpanContext().TraceID().String())
		}
		recorder := &response{ResponseWriter: w}
		next.ServeHTTP(recorder, req.WithContext(ctx))
		status := recorder.status
		if status == 0 {
			status = 200
		}
		elapsed := time.Since(started)
		r.counters["api_requests"].Add(ctx, 1, metric.WithAttributes(attribute.String("route", route), attribute.String("method", Method(req.Method)), attribute.String("status_class", Class(status))))
		if req.Method == "POST" && req.URL.Path == "/v1/events" {
			r.Count(ctx, "events", "result", EventResult(status, w.Header().Get("Idempotency-Replayed") == "true"))
			r.Duration(ctx, "acceptance_duration", elapsed)
		}
		span.SetAttributes(attribute.Int("http.status_code", status))
		category := ""
		if status >= 500 {
			category = "storage_unavailable"
		}
		Finish(span, category)
		if route != "health" {
			r.EntityLog(ctx, "request completed", "route", route, "status", status, "latency_ms", elapsed.Milliseconds())
		}
	})
}
func (r *Runtime) Dispatch(ctx context.Context, traceID, spanID string, flags byte, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if r == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	options := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(attrs...)}
	if tid, e := trace.TraceIDFromHex(traceID); e == nil {
		if sid, e := trace.SpanIDFromHex(spanID); e == nil {
			sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.TraceFlags(flags)})
			if sc.IsValid() {
				options = append(options, trace.WithLinks(trace.Link{SpanContext: sc}))
			}
		}
	}
	return r.Tracer.Start(ctx, "delivery.dispatch", options...)
}

func (r *Runtime) DispatchResult(ctx context.Context, outcome string, status int, duration time.Duration) {
	if r == nil {
		return
	}
	switch outcome {
	case "success", "retryable_failure", "permanent_failure", "cancelled", "rate_limited":
	default:
		outcome = "other"
	}
	r.counters["dispatch_attempts"].Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome), attribute.String("status_class", Class(status))))
	r.Duration(ctx, "dispatch_duration", duration)
}
