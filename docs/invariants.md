# Implementation invariants

These are project-wide acceptance obligations. Milestones 1–8 verify configuration/health, tenant/auth boundaries, atomic acceptance/idempotency, immutable bytes/snapshots, fenced worker dispatch, destination protections, lease recovery, failure experiments, idempotent replay, receiver transactional deduplication, and signing-key rotation. Optional distributed admission, shared tenant quotas/fairness, backlog admission, and additional lifecycle races are verified in milestone 7. Private metrics, linked traces, critical dependency/schema readiness, dashboards, and an operator runbook are verified in milestone 8. Retention jobs, production telemetry deployment, infrastructure failover, and load benchmarks remain planned.

1. Return event acceptance only after event, fan-out, and stored idempotency response commit together. Rollback creates no partial work.
2. Concurrent same-tenant/key requests converge through database uniqueness; matching exact request bytes return the original response, differing bytes return 409. Validate duplicate keys/trailing JSON before hashing.
3. Authentication determines tenant identity. Queries and composite foreign keys must reject cross-tenant associations without revealing other tenants' objects.
4. Exact outgoing bytes, event ID, delivery ID, and URL snapshots survive retries and replay unchanged. Endpoint changes affect future events only.
5. Claim no more work than free worker slots. Claim/attempt creation is atomic; webhook HTTP occurs outside transactions; only bounded Redis admission may precede attempt creation inside claim.
6. Only the current in-flight lease token can complete work. Recovery records abandoned attempts and respects finite attempt/age limits.
7. Disable holds future dispatch; delete cancels unfinished work. Already authorized HTTP may finish. Define and test one lifecycle lock order.
8. Rate-limit deferrals consume no attempt. Configured Redis outages fail closed while PostgreSQL retains work.
9. Revalidate destination DNS on every attempt and dial vetted addresses directly; disallow production private destinations, redirects, and inherited proxies.
10. Signing verifies exact body bytes. Receiver deduplication and its side effect commit together; duplicates remain possible across the network.
11. Retention never removes live work; expired idempotency keys may create new events. Replay requires retained events and an available endpoint.
12. Metrics have bounded labels; logs exclude payloads, credentials, secrets, and response bodies.

State transitions:

```text
pending / retry_scheduled -> in_flight
in_flight -> delivered / retry_scheduled / dead_letter / cancelled
expired in_flight -> retry_scheduled / dead_letter
pending / retry_scheduled -> cancelled
dead_letter -> pending (explicit replay, new cycle)
```


## Replay and receiver guarantees (milestone 6)

- Replay receipts are unique by tenant/delivery/key and inserted atomically with the cycle reset; same-key retries return their original acceptance despite later state changes.
- New replay keys require dead-letter state, an enabled nondeleted endpoint, and event retention checked after acquiring the event lock. Only cycle counters/deadline/state reset; lifetime counters, identity, URL snapshot, bytes, and attempts remain.
- Receiver signatures and canonical envelope fields are validated before deduplication. Namespace comes from trusted source configuration.
- Processed-event marker and demo business record commit together in receiver-owned PostgreSQL tables; an authenticated same-byte duplicate returns 204, changed bytes return 409, and unavailable persistence returns 503.
- A single local business record is an atomic database guarantee. External actions require their own idempotency/outbox; duplicate network requests remain possible.


## Scheduling and backlog (milestone 7)

- Redis denies/errors persist future eligibility without attempt/counter/lease creation; the tenant cooldown prevents scanning each queued row during one denial.
- Permission is conservative: a Redis grant lost to a database rollback/crash is not refunded or blindly retried.
- Shared scheduler rows and global tenant in-flight counts enforce configured quotas across workers; least-recent selection is approximate fairness, without preemption or a strict SLA.
- Admission serializes new ingestion/replay per tenant before counting live work. Full capacity returns 429 without committing partial fan-out, a replay cycle, or a new receipt. Existing same-key acceptances remain reusable.
- Lifecycle authorization is under tenant/endpoint locks. Already authorized requests may finish after disable; delete cancels unfinished database work and fences late completion.


## Observability boundaries (milestone 8)

- Metric labels use fixed roles/routes/methods/status classes/outcomes/categories; no tenant/event/delivery IDs, URLs, payloads, credentials, or baggage are metric labels.
- Durable queue counts are snapshots, not per-process counters. Unavailable storage emits snapshot_up=0 and omits counts instead of reporting a healthy empty queue. Dashboards gate counts/age on availability and avoid summing replicated snapshots.
- Public API does not expose metrics; operational listeners require literal loopback addresses. Collector/export errors are noncritical, while PostgreSQL/schema and configured Redis determine readiness.
- Original acceptance trace context lives outside signed bytes. Asynchronous dispatch spans link to it, and completion inherits the dispatch context even after request cancellation. Receiver headers never receive internal propagation/baggage.
- Metrics/traces are best effort, sampled or resettable observations, not the durable audit ledger; PostgreSQL event/delivery/attempt/replay history remains authoritative.
