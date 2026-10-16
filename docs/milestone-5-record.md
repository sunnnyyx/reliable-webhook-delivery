# Historical milestone 5 record

## Current milestone

5 — Failure recovery: implemented. Await the user's continuation before milestone 6 (replay and receiver correctness).

## Completed work

Milestones 0–4 established PostgreSQL-backed API/auth/endpoints, exact-byte idempotent ingestion, immutable fan-out snapshots, encrypted signing secrets, leased worker claims, fenced completion, safe outbound HTTP, a verifying receiver, and attempt history. The [milestone 4 record](milestone-4-record.md) preserves the earlier checks and limitations as historical context.

Milestone 5 adds automatic expired-lease recovery and queued-work age/budget sweeping. Every worker starts a separate maintenance loop immediately, then repeats five seconds after each pass. Each pass is bounded to 100 expired leases plus 100 aged queued rows, a five-second context, and one transaction per transition. Endpoint-before-delivery-before-attempt locking coordinates with claims/deletion. Recovery finalizes the old attempt as lease_expired and atomically clears the lease, applies full-jitter retry, or dead-letters at ten reserved attempts or 24 hours. Counters, stable IDs, URL snapshots, and outgoing bytes remain intact. Disabled and future-due queued work ages out without consuming attempts. Failure to update the exact abandoned attempt rolls back the transition.

Changed files: internal/repository/postgres/recovery.go; internal/worker/run.go and run_test.go; internal/process/run.go; migrations/00005_recovery.sql; tests/integration/recovery_test.go; README.md; docs/workers.md; this progress file and the historical milestone 4 record. No dependencies, secrets, original specification, or API contract changed.

## Verification and evidence

- Unit race suite passed, including existing retry classification/jitter/Retry-After, shutdown drain/cancellation, and new immediate maintenance startup/cancellation.
- Concurrent recovery scanners finalized eight abandoned attempts once each. Old worker completions were rejected. Batches were bounded; disabled endpoints with future-due rows aged out; no extra attempts were created.
- Ten-attempt exhaustion dead-lettered abandoned work. An intentionally inconsistent attempt record caused recovery to fail and leave the delivery in_flight, proving transition rollback. A repeated pass did not resurrect terminal work.
- Real subprocess workers were killed after committed claim and after a real local HTTP receiver persisted its effect and returned 204 before Complete. Tests use synchronization gates solely in test code and accelerated 300 ms leases, then wait for real expiry/backoff eligibility. Recovery retained ID/body and attempt history. The second case recorded two receiver effects, demonstrating ambiguous success and the need for milestone 6 transactional deduplication.
- A local receiver returned 503, 503, 429 with Retry-After: 1, then 204. Fresh worker runners and Store instances restarted after each persisted attempt. The delivery survived restarts, retained raw bytes, observed the Retry-After minimum, and finished delivered. This restart test runs within one test process; OS process kills are separate experiments.
- The two compiled worker processes still delivered twelve signed immutable payloads to the compiled receiver; both worker IDs persisted, bounded shutdown passed, and captured logs contained no payload or credentials.
- Initial subprocess tests timed out because a connection string omitted the isolated schema runtime parameter. The helper now explicitly propagates search_path; subsequent crash tests passed. No production workaround or crash switches were added.

Final full integration/build/smoke outcomes and cleanup are appended below after completion. Tests use local PostgreSQL, disposable schemas, and workspace-local Go/GOPATH/GOCACHE. No public receiver, deployment, benchmark, remote Git operation, or system installation occurred.

## Limits and risks

Receiver deduplication/replay and full key rotation scenarios remain milestone 6. PostgreSQL failure during active transactions, database failover, distributed rate limits/fairness, retention, telemetry, CI, deployment egress controls, backups, and load benchmarks remain unverified/later work. The example receiver still has no durable business side effect. Maintenance scheduling has no strict latency guarantee under database outage, saturated batches, or row contention; see [workers](workers.md) for timing components. Fencing protects persisted state but cannot undo requests already sent or ensure exactly-once effects. No public deployment is ready. Source remains local and uncommitted.

## Next small task

Milestone 6: transactional receiver deduplication, replay idempotency/history, stable identity/body across replay, preserving previous attempts, and full signing-key rotation overlap/retirement verification.

## Questions to explain

- Why does an abandoned reserved attempt count even if no request started?
- Why do recovery and endpoint deletion share lock order?
- Why must attempt finalization and lease clearing commit together?
- Why does HTTP 204 followed by worker death cause duplicate effects?
- What conditions can extend recovery beyond the ordinary lease/scan/backoff/poll timing?

## Final milestone 5 checks

On 2026-10-15, full unit race tests passed. Final full PostgreSQL integration race suite passed all 20 substantive tests plus the gated subprocess helper entry (which is a no-op outside subprocess mode). The process-kill test includes both after-claim and after-remote-acknowledgment subcases. Final gofmt/vet passed; all five binaries built; health/accounts/ingestion/two-worker binary smoke tests passed with disposable schemas. Migration 5 applied to the development database; migration round-trip passed in the integration suite. OpenAPI JSON and local documentation links are valid, the supplied original specification is byte-identical, and .env remains mode 0600. Project containers/network were stopped after tests; the database volume is preserved. No production latency or benchmark claim is made.
