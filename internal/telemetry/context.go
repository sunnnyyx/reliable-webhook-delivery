package telemetry

import "context"

type requestIDKey struct{}

// WithRequestID carries only the server-generated request ID within this process.
// It is never a metric label, durable payload field, or receiver header.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
func RequestID(ctx context.Context) string { id, _ := ctx.Value(requestIDKey{}).(string); return id }
