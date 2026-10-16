package telemetry

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"time"
)

type Snapshot struct {
	Counts           map[string]int64
	OldestDueSeconds float64
}
type Source interface {
	Observe(context.Context) (Snapshot, error)
}
type Pinger interface{ Ping(context.Context) error }

var Statuses = []string{"pending", "retry_scheduled", "in_flight", "delivered", "dead_letter", "cancelled"}

func (r *Runtime) Observe(source Source, dependencies map[string]Pinger) error {
	meter := r.Metrics.Meter("relayforge")
	queue, e := meter.Int64ObservableGauge("relayforge_queue_deliveries")
	if e != nil {
		return e
	}
	age, e := meter.Float64ObservableGauge("relayforge_oldest_due_age_seconds")
	if e != nil {
		return e
	}
	snapshotUp, e := meter.Int64ObservableGauge("relayforge_queue_snapshot_up")
	if e != nil {
		return e
	}
	dependency, e := meter.Int64ObservableGauge("relayforge_dependency_up")
	if e != nil {
		return e
	}
	traceEnabled, e := meter.Int64ObservableGauge("relayforge_traces_enabled")
	if e != nil {
		return e
	}
	traceUp, e := meter.Int64ObservableGauge("relayforge_trace_exporter_up")
	if e != nil {
		return e
	}
	_, e = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if r.traceEnabled {
			o.ObserveInt64(traceEnabled, 1)
			if r.exporterUp.Load() {
				o.ObserveInt64(traceUp, 1)
			} else {
				o.ObserveInt64(traceUp, 0)
			}
		} else {
			o.ObserveInt64(traceEnabled, 0)
		}
		for _, name := range []string{"postgres", "redis"} {
			if p := dependencies[name]; p != nil {
				bounded, cancel := context.WithTimeout(ctx, time.Second)
				err := p.Ping(bounded)
				cancel()
				up := int64(1)
				if err != nil {
					up = 0
				}
				o.ObserveInt64(dependency, up, metric.WithAttributes(attribute.String("dependency", name)))
			}
		}
		if source != nil {
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			snapshot, err := source.Observe(bounded)
			cancel()
			if err != nil {
				o.ObserveInt64(snapshotUp, 0)
				return nil
			}
			o.ObserveInt64(snapshotUp, 1)
			for _, status := range Statuses {
				o.ObserveInt64(queue, snapshot.Counts[status], metric.WithAttributes(attribute.String("status", status)))
			}
			o.ObserveFloat64(age, snapshot.OldestDueSeconds)
		}
		return nil
	}, queue, age, snapshotUp, dependency, traceEnabled, traceUp)
	return e
}
