# Historical milestone 6 record

## Current milestone

6 — Replay and receiver correctness: complete. Await the user's continuation before milestone 7.

## Completed work

Milestones 0–5 established PostgreSQL-backed API/auth/endpoints, exact-byte idempotent ingestion, immutable fan-out snapshots, encrypted signing secrets, fenced worker attempts/completion, safe outbound HTTP, finite retries, automatic expired-lease recovery, and age sweeps. See the [milestone 4](milestone-4-record.md) and [milestone 5](milestone-5-record.md) historical records for their evidence and limits.

Milestone 6 adds POST /v1/deliveries/{id}/replay and GET /v1/deliveries/{id}/replays. A new key requires dead-letter work, an enabled/nondeleted endpoint, and an unexpired event. Replay locks endpoint -> delivery -> event, checks database time after acquiring the event lock, increments the cycle/reset deadline, and inserts its audit record atomically. Stable event/delivery IDs, URL snapshot, bytes, lifetime attempts, and prior attempt history remain. A saved tenant/delivery/key acceptance returns identically after later processing/configuration changes; a new key rechecks all guards. Empty-object input prevents implicit rerouting or replay options. Replay history is tenant/delivery-bound and paginated; key values are omitted.

The receiver now requires explicit receiver-owned PostgreSQL storage and a trusted source namespace. Its migrations/version table are independent of sender migrations. Authenticated raw-body/envelope validation precedes a transaction that inserts both processed-event marker and demo business record. A valid same-byte duplicate acknowledges with 204; changed bytes under the same source/event ID return 409. Persistence failure rolls back both writes and returns 503. Readiness probes receiver storage/schema. No automatic migration, memory-only duplicate cache, sender-row foreign keys, or caller-provided tenant namespace is used.

## Changed files

migrations/00006_replays.sql; internal/delivery/replay.go; internal/repository/postgres/replay.go; internal/httpapi/replays.go and route/error wiring; internal/receiver/{handler,postgres}.go and unit tests; internal/receiver/schema SQL/provider; cmd/receiver/main.go and cmd/migrate/main.go; tests/integration/{replay,receiver}_test.go plus test-only signed crash dispatch in recovery_test.go; scripts/smoke_db.py, smoke-worker.py, smoke-replay.py; Makefile; .env.example; api/openapi.json; README and current setup/architecture/invariants/ingestion/workers guides; docs/replay-and-receiver.md; ADR 0008; this record and historical milestone 5 record. Dependencies, original specification, and ignored local secrets remain unchanged.

## Executed verification

- Concurrent same-key replay requests return one identical acceptance/new cycle; twenty distinct-key contenders admit one cycle. Old worker results cannot overwrite the new cycle. Attempt history retains all cycles. Lifetime counters, bytes, IDs, and original URL remain, even after endpoint URL changes.
- Replay-history insert failure rolls back the cycle reset. Tenant API authorization and composite foreign keys block other-tenant replay/history rows. Invalid JSON/input/key, delivered/cancelled/pending/retry/in-flight state, disabled/deleted endpoints, and expired events are rejected for new replay keys.
- Lost replay response survives a new API handler and completed delivery. The old receipt still returns identically after endpoint deletion. History pagination/cursor binding is exercised. Retention is rechecked after an observed event-row lock wait past expiry.
- Twenty concurrent signed requests with the same event and differing delivery IDs commit one receiver business record. A fresh receiver handler retains the marker; another configured source gets its independent effect. Signatures are still required on duplicates, changed raw bytes return 409, and canonical envelope fields/duplicates are validated before storage.
- A trigger-injected receiver business-write failure returns 503 and leaves no marker; retry succeeds after the test trigger is removed. Receiver storage closure makes readiness and valid duplicate requests return 503. Receiver migration up/repeat/down/up preserves sender tables.
- A real worker subprocess is killed after production signing/transport receives a real receiver 204 but before local completion. After real accelerated lease expiry/backoff, recovery sends again: two requests, one business record, unchanged ID/body hash, and lease_expired/success attempt history. Crash gates are confined to test code; production has no failpoint environment options.
- Rotation tests use a queued-before-rotation event and old-only, both, and new-only receiver configurations during overlap. They verify two signatures during overlap, only the new signature after retirement, failure at an old-only receiver, successful replay after configuration repair, and rejection of old-signed requests by a new-only receiver. Retirement is accelerated by isolated test SQL while preserving active/retire timestamp constraints. Published Standard Webhooks vector/tamper/timestamp tests remain passing.
- The compiled replay smoke exercises actual API/worker/receiver processes: permanent 400 -> twenty same-key API calls -> one cycle -> delivered, retained raw bytes and both histories, one audit row, duplicate acknowledgment with one receiver record, clean logs and graceful shutdown.
- The two-worker compiled smoke still dispatches twelve immutable signed events; restarting the compiled receiver and redelivering all twelve keeps exactly twelve business records. Sender/receiver smoke schemas are separate and disposed after tests.

## Corrections during implementation

The initial accelerated retirement fixture violated the existing retire_after > active_from constraint. The test now moves the old secret's active_from and retire_after together to valid past times. No constraint was weakened. Receiver storage configuration is explicitly required, and smoke helpers now provision separate receiver schemas rather than relying on sender database defaults. Replay reads time in a fresh query after its event-row lock to close the retention lock-wait boundary.

## Limits and next work

One local database business record per source/event is an atomic receiver guarantee. External payments, mail, or remote effects still require downstream idempotency or an outbox. Signature verification and deduplication solve separate problems. Source namespaces bind one tenant/source to its accepted keys; multi-tenant receiver routing is not implemented. Retired keys must also be removed from receiver configuration; sender retirement alone does not revoke a key that a receiver continues trusting.

Receiver markers and replay receipts currently have no cleanup. Retention jobs must preserve markers throughout possible retries/replay/live work and audit receipts throughout retained history. Production receiver ACLs/separate roles, PostgreSQL failover/restore, telemetry, distributed rate limits/fairness/backlog limits, CI/deployment/egress enforcement, and load benchmarks remain later/unverified work. Local tests use separate schemas on one PostgreSQL service, not a provisioned production isolation boundary. Source is local/uncommitted with no remote; no hosted deployment or performance claims were introduced.

Next: milestone 7 — rate limits and lifecycle races, optional Redis behavior, distributed deferral scheduling, basic tenant fairness, backlog limits, and related outage/disable/delete concurrency checks. The dashboard remains later.

## Questions to explain

- Why is replay an idempotent new cycle instead of a new delivery/body?
- Why should the same replay key return its old acceptance after delivery completes?
- Why does receiver deduplication use event ID plus a trusted source namespace?
- Why must marker and business write share a transaction?
- Why must a duplicate still pass signature checks?
- Why are marker retention and key retirement receiver responsibilities too?


## Final milestone 6 checks

On 2026-10-15, the full unit race suite and final PostgreSQL integration race suite passed: 29 substantive integration tests plus the gated test-helper entry. Final gofmt/vet passed, all five binaries built, and all five binary smoke targets passed (health, accounts, ingestion, two-worker/receiver restart, replay). The replay binary smoke was rerun after the retention post-lock-clock fix. Sender migration 6 applied to development PostgreSQL. Receiver migrations passed independent up/repeat/down/up checks; the CLI requires an explicit receiver URL and rejects production receiver rollback before connecting.

OpenAPI 0.6 JSON/internal references, documentation links, Python syntax, unchanged original specification, .env mode 0600, and private-value exclusion from source passed. Smoke helpers disposed all generated sender/receiver schemas; a final database query found zero smoke schemas. Child processes were stopped by their helpers. make dev-down stopped only this project's containers/network and preserved its volume. The receiver's manual setup still requires an explicitly configured receiver-owned database/source/endpoint secret; smoke checks provision disposable equivalents without changing the user's ignored secrets. No deployment, remote Git operation, system installation, dependency change, or fabricated benchmark was introduced.
