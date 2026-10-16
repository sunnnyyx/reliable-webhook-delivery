# 0010 — Private metrics, bounded traces and durable correlation

Status: accepted, milestone 8.

Use official OpenTelemetry Go SDKs, Prometheus export on operational listeners, and optional OTLP/HTTP trace export. Enforce literal loopback for API operations/worker listeners and never mount metrics on public API routes. Metric labels are bounded enums; object IDs belong in authenticated views, logs and private traces. Counters are best effort process observations; PostgreSQL remains authoritative. Aggregate queue snapshots expose availability and omit data on failure; dashboards avoid replica double-counting and false empty queues.

Store original acceptance span context in a separate tenant-aware table, leaving signed bytes immutable. Public incoming trace headers/baggage are ignored. Dispatch starts a new root linked to acceptance; completion stays its child even when the send context is cancelled. Do not auto-instrument arbitrary receiver HTTP or inject internal context/baggage/authorization. Export is sampled, asynchronous and bounded; collector errors are sanitized and noncritical. Critical schema/connectivity and configured Redis determine readiness.

Provide a reproducible local Prometheus/Grafana/Jaeger profile, provisioned dashboard/alerts, and a tested operator runbook. Copy reviewed assets into a project-owned Docker volume to avoid host configuration bind-mount dependency. Jaeger's current query API is v3 OTLP JSON; parse streamed results rather than assuming legacy UI endpoints. Local storage/credentials are ephemeral development fixtures, with no production deployment, notification routing, HA or benchmark claim.

Versions and primary references are in [observability](../observability.md). Tests verify live operator diagnosis/replay, all panel queries, acceptance/dispatch links, private exposure, bounded labels, no payload/secret/baggage leakage, snapshot failure, and noncritical collector outage.
