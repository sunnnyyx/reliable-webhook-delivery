# Project progress

## Current milestone

8 — Operational visibility: complete. Await the user's continuation before milestone 9, the optional product interface.

There are 12 milestones numbered 0–11. After milestone 8, milestones 9–11 remain: optional UI, benchmarks/hardening, deployment/portfolio.

## Completed work

Milestones 0–7 established PostgreSQL-backed API/auth/endpoints, atomic exact-byte ingestion/fan-out, immutable payloads/URL snapshots, encrypted signing secrets, fenced worker attempts/completion, protected HTTP, finite retries/recovery, replay history, transactional receiver deduplication/rotation, optional Redis admission, tenant quotas/fairness, backlog limits, and lifecycle checks. See the [milestone 7 record](milestone-7-record.md) for earlier evidence and historical links.

Milestone 8 adds official OpenTelemetry metrics and optional OTLP/HTTP traces. The public API does not expose metrics; API operations and worker health/metrics require literal loopback listeners. API operational snapshots aggregate durable delivery state and overdue eligible work, with explicit availability. Process counters/histograms record bounded request/event/dispatch/completion/replay/recovery/deferral categories and timing; identifiers/URLs/payloads/credentials/baggage are not metric labels. Configured Redis admission retains its counters, now with the fixed role label.

Server-generated request IDs also propagate through internal contexts into entity logs when tracing is disabled. Original acceptance span context is persisted transactionally in a separate tenant-aware table. Dispatch spans are new roots linked to that acceptance across retries/replay; completion is a child of dispatch even after send cancellation. Public incoming propagation headers are ignored and receiver headers remain explicitly allowlisted. IDs appear in private traces/structured logs and authenticated history views; no raw payload/secret/SQL error or receiver URL is attached to spans. Event detail optionally returns original trace_id; response X-Trace-ID describes the current request. Signed outgoing bytes remain unchanged.

Trace export is explicit, sampled, asynchronous, bounded and sanitized. Export failure affects telemetry availability, not durable API/worker readiness. PostgreSQL connectivity plus required sender schema determine readiness; configured Redis is also critical for workers. Unavailable snapshots emit snapshot_up=0 and omit counts rather than a false empty backlog. Metrics/traces are best-effort observations, not a durable audit ledger.

The optional local monitoring profile provisions Prometheus, a 16-panel Grafana dashboard, four example alert rules, and Jaeger. Reviewed assets are copied into a project-owned Docker configuration volume, avoiding host bind mounts. Services publish only on loopback; local runtime telemetry is ephemeral. The [operator runbook](runbook.md) documents finding events, failure diagnosis, backlog/dependency queries, replay, trace/log correlation, and outages. [Observability](observability.md) defines each metric, clock/sampling/replica semantics, configuration and limits.

## Changed files

internal/telemetry/{runtime,http,observe,context}.go and tests; migrations/00008_trace_context.sql; Store telemetry hooks/wrappers and aggregate observation; ingestion/replay/completion/recovery/claim correlation changes; Job trace/time metadata; event-detail trace ID view; API request instrumentation and schema-aware health; worker dispatch/completion/log context; Redis metrics hooks; config/process private operational listeners and bounded shutdown; tests/integration/telemetry_test.go; config tests; deploy/observability assets/dashboard/provisioning/alerts and optional Compose services; scripts/observability-assets.py, smoke-observability.py, isolated API-ops ports in smoke_db.py, role-label-aware rate smoke assertion; Makefile; .env.example; go.mod/go.sum; OpenAPI 0.8; README/current guides; observability.md/runbook.md/ADR 0010; this log and historical milestone 7 record. The supplied original specification and ignored local secrets remain unchanged.

Dependencies verified and pinned: OpenTelemetry Go SDK/OTLP trace exporter v1.47.0 and Prometheus exporter v0.69.0, plus their pinned indirect modules. Existing Go/PostgreSQL/pgx/Goose/Redis pins remain. Local images verified against official releases and pulled: Prometheus v3.15.0, Grafana 13.2.4, Jaeger 2.22.0. Primary references are in observability.md. Tags are pinned by version, not digest.

## Executed evidence

- Unit race checks cover bounded route/method/status/category dimensions, ignored public trace parent/baggage, private listener/config validation, unavailable snapshot omission, sanitized exporter errors, and noncritical request behavior. Existing suites continue passing.
- A real PostgreSQL/API/worker/HTTP integration test accepts and idempotently retries an event, rejects invalid/unauthorized requests, records a permanent 400/dead-letter snapshot, replays, and completes 204. It checks all original bytes, event/delivery identity, counters, first-attempt/ack timing observation counts, stale completion, and the original acceptance links on both worker roots. Completion spans retain dispatch context. Metadata/metrics are inspected for payload, key, URL, baggage and metric-ID exclusion.
- Deferral/recovery integration checks distinguish rate denial from attempts and count both age sweeps and abandoned leases only after commit. Closing storage makes dependency/snapshot availability zero and omits queue values; no raw connection details appear. Renaming a required table changes readiness 200 -> 503 -> 200 when restored, without disclosing schema names.
- A real API/PostgreSQL test uses a collector returning 503. Trace-export availability drops, later API submissions remain 202/durable, and readiness remains healthy. No collector response diagnostic is returned to callers.
- The compiled operator smoke uses actual API/worker/receiver processes and the live monitoring stack: find an event/HTTP 400 attempt, observe dead letter in Prometheus, retrieve its acceptance trace, replay once, observe delivery, and confirm one receiver business record. Public /metrics returns 404; private metrics respond. Replay bytes remain identical and receiver headers contain no trace/baggage/API authorization.
- Grafana's provisioned dashboard loads through its authenticated API, all 16 panel PromQL expressions evaluate successfully in live Prometheus, and both configured scrape targets are healthy while fixture processes run. Jaeger's v3 OTLP JSON query returns both matching delivery.dispatch roots with links to the original acceptance trace. This validates configuration/query integration, not a pixel-rendered screenshot or performance/SLO result.
- promtool validated the exact mounted Prometheus configuration and all four alert rules. No notification channel/Alertmanager is deployed, and alert firing/delivery is not claimed.
- All prior health/accounts/ingestion/two-worker/receiver-restart/replay/Redis-outage smoke targets pass with telemetry. Go race/integration and final outcomes are recorded below.

## Corrections during implementation

Initial Prometheus/Grafana host configuration bind mounts stalled before startup. Only those project containers were removed, and reviewed assets were copied via docker cp to a project-owned named volume; both services then started successfully without restarting Docker or unrelated services. The first live check timed out while services were stalled. The final compose default profile still contains only PostgreSQL and its volume; optional observability assets are not required for ordinary dev-up.

An older rate-limit smoke expected unlabeled counter lines and missed the new fixed role label; it now accepts the labeled series. The live trace search initially assumed Jaeger's legacy /api/traces endpoint; current Jaeger serves v3 OTLP JSON/streamed responses. The smoke now verifies actual v3 span contents and links, rather than treating a 200 UI fallback as trace evidence. An unverified Grafana Jaeger datasource was removed; traces are viewed in Jaeger's own UI. No failed initial check is presented as a passing final result.

## Limits and next work

Tracing defaults off until an explicit endpoint is configured; metrics remain on. Sampling, bounded queues, restarts, exporter outages and backend retention can drop observations. Counters are not audited totals. Cross-boundary wall-clock durations assume synchronized clocks and clamp negative values. API queue snapshots are global and must not be summed across replicas; expensive large-history aggregation/scaling benchmarks remain milestone 10. Snapshot failure and critical readiness probes have distinct meanings.

The local monitoring profile is ephemeral and uses development credentials. Docker Desktop host-to-loopback access was verified here; other engines may need host-native Prometheus/private networking, without widening app binds. Production monitoring authentication/TLS/ACLs, durable storage, HA/backups, notification routing, failover/restore, retention jobs, master-key rotation, CI, deployment/egress controls, and load benchmarks remain later/unverified work. Grafana panel queries/provisioning were validated; no screenshot/pixel-layout or actual alert firing/notification test is claimed. Source is local/uncommitted with no remote or hosted deployment.

Next: milestone 9 — optional small authenticated product interface after the verified backend. Milestone 10 covers benchmarks/hardening, and milestone 11 deployment/portfolio. Stop here until continuation.

## Questions to explain

- Why are queue snapshots different from per-process counters, and why does snapshot failure not mean zero backlog?
- Why must metric dimensions be bounded while IDs can appear in private traces/logs?
- Why link asynchronous dispatch roots to acceptance rather than forwarding producer baggage to receivers?
- Why is collector export noncritical while PostgreSQL/schema and configured Redis determine readiness?
- What do sampling, batch limits, process death and clock skew mean for observability claims?
- How can an operator find a failed event, explain its attempt history, observe backlog and replay without mutating database state manually?


## Final milestone 8 checks

On 2026-10-15, final unit race tests and the full PostgreSQL/Redis integration race suite passed all 40 substantive tests plus the gated helper entry. Final gofmt/vet passed, and all five binaries built. All seven binary smoke targets passed: health, accounts, ingestion, two-worker signed delivery/receiver restart, replay, Redis outage/recovery, and live observability/operator replay. Following the request-ID-without-tracing correlation addition, unit race tests, the full integration race suite, and ingestion/live-observability smoke targets passed again. The observability smoke also passed with a blank GRAFANA_ADMIN_PASSWORD, matching the documented local default. Sender migration 8 applied to development PostgreSQL; migration round trips passed in the integration suite.

promtool validated the exact mounted config and all four rules. Live Grafana provisioning/all 16 panel queries and actual Jaeger v3 acceptance/dispatch contents/links passed. OpenAPI 0.8 internal references, documentation links, dashboard JSON, Python syntax, unchanged original specification, .env mode 0600, and private-value exclusion from source passed. The Compose default profile was verified to contain only PostgreSQL/postgres_data, so optional configuration volumes do not block ordinary setup.

The final database query found zero generated smoke schemas. Helpers stopped fixture processes/proxies. dev-down removed only this project's PostgreSQL/Redis/Prometheus/Grafana/Jaeger containers and network, preserving PostgreSQL and the generated observability configuration volume. Local monitoring runtime data is intentionally ephemeral. No ignored secrets were changed, tracing was not silently enabled in the user's .env, and no hosted deployment, public telemetry endpoint, notification routing, remote Git operation, commit, system installation, or fabricated benchmark was introduced.
