# Operational visibility

## Start the local stack

```sh
make dev-observe-up
make migrate-up
```

The optional profile starts version-pinned Prometheus, Grafana, Jaeger, PostgreSQL, and Redis. It copies the reviewed deploy/observability assets into a project-owned Docker configuration volume, mounted read-only by monitoring services. No host configuration bind mounts are needed. After editing assets, rerun preparation and restart the affected monitoring service to load changed startup configuration. The config volume and PostgreSQL data survive dev-down; local Prometheus/Grafana/Jaeger runtime data is ephemeral. This is a local development demonstration, not a production monitoring deployment or backup plan.

| Local surface | Default address |
|---|---|
| API operational metrics/health | http://127.0.0.1:9091 |
| Worker metrics/health | http://127.0.0.1:8081 |
| Prometheus | http://127.0.0.1:19090 |
| Grafana | http://127.0.0.1:13000 |
| Jaeger UI | http://127.0.0.1:16686 |
| OTLP/HTTP traces ingestion | http://127.0.0.1:14318/v1/traces |

Grafana's local-only initial login is admin / relayforge-local-only unless GRAFANA_ADMIN_PASSWORD is set. Set a deliberate password before using a shared machine. All published stack ports bind loopback. Sign-up/usage reporting are disabled. These defaults are development fixtures; the stack has no public ingress, alert routing, production auth/TLS, durable trace backend, or HA/backup configuration.

API_OPS_ADDR and WORKER_ADDR must use literal loopback IPs, including ::1 if desired. The public API has no /metrics route. API_OPS_ADDR defaults to 127.0.0.1:9091; worker operations retain 127.0.0.1:8081. Prometheus's supplied targets match these defaults. A second worker needs another private port and an explicitly added scrape target. Each process exposes role api/worker as a constant label; Prometheus adds its job/instance scrape labels.

The local Compose targets use Docker Desktop's host.docker.internal to reach host loopback listeners. This was verified here on macOS Docker Desktop. Linux/other engines may not reach a host loopback listener through host-gateway; run Prometheus on the host or use a separately designed private operational network. Do not expose metrics on all interfaces to make a local demo work. The observability smoke needs default operational ports 9091/8081 free, plus the running stack; it refuses occupied ports.

## Optional traces

Set these in ignored .env before running API and worker:

```dotenv
TRACE_ENDPOINT=http://127.0.0.1:14318/v1/traces
TRACE_SAMPLE_RATIO=1
```

TRACE_ENDPOINT must be the full explicit OTLP/HTTP traces URL. Blank disables trace export/context generation while metrics remain enabled. TRACE_SAMPLE_RATIO defaults to 0.1, validated in [0,1]; use 1 for a short diagnostic demo. Production URLs require HTTPS and cannot embed credentials, query, or fragment. No default remote endpoint is selected. The exporter uses bounded HTTP without proxy inheritance/redirects or inherited OTLP headers.

The API deliberately starts internal roots and ignores public traceparent/tracestate/baggage. It returns optional X-Trace-ID for the current request. Acceptance stores its internal span context in event_trace_context, separate from immutable signed event bytes. GET /v1/events/{id} returns the original acceptance trace_id if present. A retry's X-Trace-ID can differ from that original event trace. Sampling, exporter loss, or backend retention mean a valid trace ID does not guarantee a trace exists in the backend.

Each delivery.dispatch span is a new root with a link to original acceptance, across retries/replays and API/worker restarts. delivery.complete is its child. event.accept, delivery.replay and committed delivery.recover operations add diagnostic spans. Tenant/event/delivery/worker IDs and cycle/attempt numbers are allowed in private traces/logs, not metric labels. Payloads, API keys, signing secrets, receiver URLs, SQL error strings and baggage are not recorded. Receiver HTTP retains the explicit signing/content header allowlist; internal trace context is never injected.

The batch export queue holds at most 512 spans, batches at most 128, waits up to one second, and bounds exports to two seconds without retry. Shutdown gives telemetry up to three additional seconds after active work drains. Sampling and a full queue can drop traces. Export errors are sanitized/counted and set trace_exporter_up=0 after a failed export; a successful export restores it. API/worker readiness does not depend on the collector. The exporter-up gauge starts optimistic when configured and reflects exports, not an independent health probe. Correlation metadata still participates in the acceptance database transaction; critical schema/storage errors remain ordinary durable-storage failures.

## Metric definitions

All metrics are exported through the official OpenTelemetry Prometheus exporter. Histogram names end in _seconds, with standard _bucket/_sum/_count series. Labels use fixed enums. There are no object-ID/URL labels or raw-error labels, no default Go/process collector, and no process-instance ID generated as a metric label. Dashboards identify replicas through the scrape target.

| Metric | Meaning / bounded labels |
|---|---|
| relayforge_api_requests_total | API requests by route, method, status_class |
| relayforge_events_total | POST /v1/events result: accepted, idempotent, invalid_input, unauthorized, not_found, idempotency_mismatch, payload_too_large, backlog_limit, other, storage_unavailable |
| relayforge_acceptance_duration_seconds | Complete event-submission request duration, including rejected requests |
| relayforge_dispatch_attempts_total | Dispatcher results by bounded outcome and HTTP status_class; local preflight failures may have class none |
| relayforge_dispatch_duration_seconds | Dispatcher elapsed time, including preparation/HTTP/limited response read |
| relayforge_first_attempt_queue_delay_seconds | Event creation to initial cycle's first dispatch, once per delivery; replay cycles do not add another initial sample |
| relayforge_event_to_ack_duration_seconds | Original event creation to a successful committed delivery-cycle completion; fan-out/replay can add multiple samples |
| relayforge_completions_total | committed, stale, storage_error; separates remote result from persistence |
| relayforge_replays_total | Repository replay requests by accepted/idempotent/bounded rejection category |
| relayforge_recoveries_total | Committed lease_expired/cycle_expired/attempt_budget_exhausted transitions; lease expirations can end in retry or dead letter |
| relayforge_rate_limit_deferrals_total | Committed local rate_limited/limiter_unavailable deferrals, without attempts |
| relayforge_rate_limit_permissions_total / denials_total / errors_total | Redis admission outcomes when enabled; a grant is not a successful send |
| relayforge_queue_deliveries | Durable delivery count by six status values; API operational listeners only |
| relayforge_oldest_due_age_seconds | How far the oldest queued next_attempt_at is overdue, among enabled/nondeleted endpoints, active tenants, finite eligible cycles; ignores quota/rate cooldown so admission bottlenecks remain visible |
| relayforge_queue_snapshot_up | 1 when durable counts/age were collected, 0 if query/storage unavailable |
| relayforge_dependency_up | Bounded scrape-time checks of postgres and configured redis |
| relayforge_traces_enabled | Whether tracing is configured |
| relayforge_trace_exporter_up | Last export availability, present only when tracing is configured |
| relayforge_telemetry_export_errors_total | Failed trace-export batches, signal=traces |

Counters/histograms are process-local, best effort, reset on restart, and may miss an observation if a process dies after commit. PostgreSQL history is the audit source. No traffic means some counter/histogram series do not exist yet; rates need multiple scrapes and p95 can be NaN without observations. No zero traffic or latency is fabricated for empty series. Acceptance/dispatch elapsed durations use local monotonic time. Queue/event-to-ack durations cross application/database wall clocks, clamp negatives to zero, and assume synchronized clocks; they are not benchmark evidence.

Queue snapshots scan aggregate durable state under a two-second context, without locking work. Critical scrape probes are bounded to one second per dependency. On snapshot failure, counts and age are omitted and snapshot_up=0; do not interpret failure as an empty backlog. Snapshots include terminal historical rows as well as live work. The same global data is visible at multiple API replicas, so dashboard queries use max by(status), not sum. Counts/age are gated on snapshot_up=1. Multi-statement event/history views and snapshots are diagnostic observations, not a transactional scheduling guarantee. Load/scale and scrape overhead benchmarking remain milestone 10.

## Readiness and dashboards

API readiness probes PostgreSQL and required sender tables. Worker readiness also probes configured Redis. Receiver readiness checks receiver-owned schema independently. Liveness performs no dependency probe. Redis outage holds new dispatch while API acceptance can continue up to its backlog cap. Collector outage affects trace export, not readiness. Queue snapshot failure remains explicitly visible even if a separate connectivity/schema probe succeeds.

The provisioned Grafana dashboard has 16 panels covering acceptance, durable state/age, dispatch outcomes/durations, queue delay, acknowledgment duration, recovery, completion persistence, rate limiting, replay, critical dependencies, snapshot availability, and trace-export health. The Prometheus config includes four example alerts for scrape/dependency loss, overdue work and lease recovery. Thresholds are local examples, not production SLOs; no Alertmanager/notification channel is configured. Jaeger is accessed directly through its UI; no unverified Grafana Jaeger plugin integration is promised.

```sh
# Validate the exact mounted configuration/rules:
docker compose --env-file .env -f deploy/compose.yaml --profile observability \
  exec -T prometheus promtool check config /etc/relayforge/prometheus.yml
make test-observability-smoke
```

The smoke launches compiled API/worker/receiver on isolated PostgreSQL schemas, submits one event, diagnoses a permanent 400 through event/delivery/attempt views, observes dead letter in live Prometheus, retrieves its acceptance trace, replays once, and observes delivery. It evaluates every provisioned panel query and retrieves both linked dispatch traces through Jaeger's v3 OTLP JSON query API. The receiver gets no internal auth/trace/baggage headers and commits one business record. Processes/schemas are cleaned up; monitoring data is synthetic and not a benchmark.

Dependencies verified against primary references: [OpenTelemetry Go v1.47.0 / Prometheus exporter v0.69.0](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0), [official Go exporters guide](https://opentelemetry.io/docs/languages/go/exporters/), [Prometheus v3.15.0](https://github.com/prometheus/prometheus/releases/tag/v3.15.0), [Grafana v13.2.4](https://github.com/grafana/grafana/releases/tag/v13.2.4), [Jaeger v2.22.0](https://github.com/jaegertracing/jaeger/releases/tag/v2.22.0). Container tags are version-pinned, not digest-pinned. See the [runbook](runbook.md) for diagnosis and replay commands.
