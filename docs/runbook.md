# Operator runbook

Work from the repository root with the correct tenant API key, event ID, and local API address. Use authenticated tenant history to diagnose individual objects; private metrics describe aggregate behavior. PostgreSQL is authoritative. Keep keys and payloads out of shared logs/tickets; the event-detail response includes payload, so select only necessary metadata when sharing diagnostics.

## Find an event and its deliveries

If event submission lost its response, resend the same exact input bytes and Idempotency-Key within ingestion's seven-day window. A committed original acceptance returns its original event_id; do not invent a new key to work around an ambiguous result. If you already have the event ID:

```sh
curl -sS "http://127.0.0.1:8080/v1/events/$EVENT_ID" \
  -H "Authorization: Bearer $TENANT_KEY" | jq '{event,trace_id,delivery_summary}'
curl -sS "http://127.0.0.1:8080/v1/deliveries?event_id=$EVENT_ID&limit=25" \
  -H "Authorization: Bearer $TENANT_KEY"
```

Follow next_cursor for further pages. Other tenants' IDs return 404. No event ID available? GET /v1/events?limit=25 lists the tenant's recent metadata; original producer input is not a global search index. The acceptance receipt event_id and server-generated request_id are the best starting points.

## Explain one failure

```sh
curl -sS "http://127.0.0.1:8080/v1/deliveries/$DELIVERY_ID" \
  -H "Authorization: Bearer $TENANT_KEY"
curl -sS "http://127.0.0.1:8080/v1/deliveries/$DELIVERY_ID/attempts?limit=25" \
  -H "Authorization: Bearer $TENANT_KEY"
curl -sS "http://127.0.0.1:8080/v1/deliveries/$DELIVERY_ID/replays?limit=25" \
  -H "Authorization: Bearer $TENANT_KEY"
```

Read cycle/attempt numbers, outcomes, HTTP class, bounded error code, next_attempt_at, lease expiry, and cycle/lifetime counts. Started means reserved work, not proof of an HTTP send. success means remote acknowledgment, while delivered additionally means fenced completion committed. A lease_expired attempt is ambiguous: the receiver may already have committed. Local rate_limited/limiter_unavailable scheduling creates no attempt and consumes no budget; last HTTP status can still describe an earlier real request.

| Observation | Action |
|---|---|
| retry_scheduled / retryable_failure | Inspect next time and history; 408/425/429/5xx/network failures retry within ten attempts/24 hours |
| dead_letter / http_permanent or redirect_blocked | Repair receiver behavior/configuration; redirects are intentionally not followed |
| forbidden_destination or tls_validation_failed | Inspect destination/DNS/certificate policy; do not enable private-network bypasses or disable TLS verification |
| in_flight past lease expiry | Check worker health/recovery logs; normal maintenance is bounded and repeats every five seconds; do not edit lease tokens manually |
| endpoint disabled / future due work | Re-enable only when intended; disabling holds claims while age limits continue |
| cancelled / endpoint_deleted | Terminal cancellation; replay is forbidden |
| limiter_unavailable / no attempt | Check private Redis connectivity/readiness; preserve PostgreSQL work and fail-closed behavior |
| backlog_limit 429 on ingestion/replay | Wait for live capacity to free; retry the same key; no partial request committed |
| stale completion rejected | A newer state/token/deletion won; inspect histories and receiver deduplication rather than overwriting state |

The original URL snapshot is immutable. Changing an endpoint URL affects new events; if an old URL is irreparable, a new event is required to route to the changed URL. Replay will not silently redirect old work.

## Observe backlog and dependencies

```sh
curl -sS http://127.0.0.1:9091/health/ready
curl -sS http://127.0.0.1:8081/health/ready
curl -sS http://127.0.0.1:9091/metrics
curl -sS http://127.0.0.1:8081/metrics
```

Use the local Grafana RelayForge operations dashboard or Prometheus queries:

```promql
max by (status) (relayforge_queue_deliveries and on(job,instance,role) (relayforge_queue_snapshot_up == 1))
max(relayforge_oldest_due_age_seconds and on(job,instance,role) (relayforge_queue_snapshot_up == 1))
sum by (outcome,status_class) (rate(relayforge_dispatch_attempts_total[5m]))
sum by (reason) (rate(relayforge_rate_limit_deferrals_total[5m]))
sum by (result) (increase(relayforge_completions_total[5m]))
min by (dependency,role) (relayforge_dependency_up)
```

Do not sum global queue gauges across API replicas. A missing/failed snapshot is unknown, not zero; inspect snapshot_up and scrape up. Rate/p95 panels require repeated scrapes and observations. Rising age with healthy receivers can mean workers/quota/limiter are the bottleneck. Rising permanent failures need receiver/destination repair; raising concurrency does not fix a permanent 400. Use the documented [metric meanings and limits](observability.md).

## Replay safely after repair

Only dead-letter deliveries with an enabled/nondeleted endpoint, unexpired retained event, and backlog capacity can start a new cycle:

```sh
curl -sS -X POST "http://127.0.0.1:8080/v1/deliveries/$DELIVERY_ID/replay" \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: operator-repair-1' --data '{}'
```

202 is a replay acceptance receipt, not proof of success. Save its id and new_cycle_number. If the response is ambiguous, retry that same key/body; it returns the same receipt even if delivery progresses. Use a distinct key only for an intentionally new replay after a later dead letter. Poll delivery state/attempts and inspect replay history. Do not replay delivered/in-flight/cancelled work; it returns 409. Disabled/expired work also rejects new keys. A 429 means capacity unavailable, not accepted replay. Old attempts, event/delivery IDs, raw body, and original URL remain.

Receiver transactional deduplication should keep a previously processed event to one local business record. External effects require downstream idempotency or an outbox. Never delete receiver markers or change its source namespace to force a repeat effect.

## Follow linked traces and logs

With TRACE_ENDPOINT configured, X-Trace-ID identifies the current API request, while event-detail trace_id identifies original acceptance. In Jaeger, look up that trace ID; search service relayforge-worker with tags event_id or delivery_id to see dispatch roots and their acceptance links. Completion spans are children of dispatch. Replays preserve original event links. Request completion logs join request_id to trace_id; acceptance/recovery/attempt logs add tenant/event/delivery/cycle/worker identifiers. No payload/secret logging is needed for diagnosis.

A valid trace ID may be unsampled, dropped during export outage, or outside backend retention. PostgreSQL attempt/replay records remain available. Check relayforge_traces_enabled, relayforge_trace_exporter_up and relayforge_telemetry_export_errors_total; exporter-up reflects recent export outcomes, not a separate collector probe. The SDK emits sanitized errors. Do not paste raw environment, signing keys, or full event payload into telemetry to fill a missing trace.

## Dependency and deployment incidents

- PostgreSQL/schema unavailable: API/worker readiness becomes 503. New acceptance cannot be acknowledged without commit. Check local service health and apply migrations with make migrate-up before launching processes. Restore connectivity and retry ambiguous API submissions with their original bytes/key. Infrastructure backup/restore/failover is not implemented by this local demo.
- Redis unavailable when enabled: worker readiness becomes 503, liveness remains healthy, and dispatch defers without attempts. API can still accept up to backlog capacity. Restore Redis; the same running workers resume after cooldowns. Do not silently disable rate limiting, clear keys, or change prefix. State loss may grant a fresh burst. See [rate-limit tradeoffs](rate-limits.md).
- Collector/monitoring unavailable: delivery remains independent. Restore the trusted collector or local profile, check exporter errors, and expect possible sampled/export losses. No durable trace-export outbox exists. Prometheus/Grafana/Jaeger development storage is ephemeral.
- Worker stopping/scaling: SIGTERM stops claims, drains within configured grace, then cancels HTTP; completion and telemetry have their own bounded final steps. Abrupt kills leave durable leases for recovery. Replicas must share database/encryption key/rate settings and use distinct private operational ports/scrape targets. Quotas may intentionally leave slots idle.
- Key rotation/revocation: follow [rotation workflow](replay-and-receiver.md), update receiver keys during overlap, and preserve its namespace/database. Revoke leaked API keys with the private admin CLI. Master encryption-key rotation tooling and a production secret manager remain deployment work; do not replace ENCRYPTION_KEY casually.

Do not manually mark delivered, refund attempt counters, mutate snapshots, delete live events, or bypass lease fencing to clear a dashboard. Retention cleanup and production recovery/deployment procedures remain later work. The local stack's default credentials/loopback ports are development fixtures; no public observability endpoint or notification service has been deployed.
