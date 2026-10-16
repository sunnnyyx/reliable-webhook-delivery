# Historical milestone 4 record

## Current milestone

4 — Worker happy path: complete. Await the user's next continuation before milestone 5 (failure recovery).

## Completed tasks

- 0–1: repository/spec/ADRs, configuration, health/API/worker processes, logging/shutdown, PostgreSQL Compose/migrations, Make targets, real database checks.
- 2: admin/tenants, hashed API keys and auth/revocation, tenant-isolated endpoint CRUD, encrypted signing secrets/rotation records, cursor pagination.
- 3: atomic event acceptance/idempotency/fan-out, immutable payloads/URL snapshots, tenant-scoped event/delivery views, deletion/cancellation coordination.
- 4: started-attempt migration/history; endpoint-first due claims with fresh lease tokens; capacity-bounded worker slots; safe post-commit HTTP transport; HMAC signing/current active keys; fenced atomic completion; verifying sample receiver; attempt API pagination; bounded failure scheduling; two-process binary delivery test; updated docs/OpenAPI.

## Changed files in milestone 4

migrations/00004_attempts.sql; internal/netpolicy/policy.go/tests; internal/signing/signing.go/tests; internal/delivery/work.go/tests and attempt.go; internal/repository/postgres/claim.go and attempt_views.go; Store policy and endpoint validation/lifecycle cancellation updates; internal/worker/{run,sender}.go/tests; internal/receiver/handler.go/tests; cmd/receiver/main.go; config/process wiring and worker config tests; HTTP attempt routes; tests/integration/worker_test.go; scripts/smoke-worker.py and smoke_db.py; existing smoke isolation; Makefile build/receiver/smoke targets; .env.example/.gitignore; api/openapi.json; README/current guides/architecture/invariants; docs/workers.md and ADR 0006; this log. Original specification and existing ignored local secrets are preserved.

## Decisions and reasons

No new dependencies: Go 1.27.2, pgx v5.11.0, Goose v3.29.0, PostgreSQL 18.6-alpine3.24 remain pinned.

One loop per free worker slot claims one delivery and immediately sends it. Claim locks active endpoint before due delivery with SKIP LOCKED, loads stored bytes/current active encrypted keys, reserves a token/lease, increments counters, and inserts a started attempt in one transaction. Commit precedes decryption/DNS/TCP/TLS/HTTP. A reserved attempt can be consumed even if no HTTP request starts. Endpoint mutation/delete/claim follow endpoint-before-delivery order; disabled endpoints/tenants are not claimed. Deletion cancels unfinished delivery and started attempt records, with explicit cancelled outcome.

Completion locks the delivery before checking token/state/DB-clock expiry, so time spent waiting for another transaction cannot admit an expired lease. Delivery and attempt update together; any mismatch/failure rolls back. A stale/expired/deleted worker cannot overwrite newer state; fencing does not undo a remote request or eliminate duplicates.

Dedicated HTTP/1 transport: no environment proxy, redirects, cookie jar, caller headers, or pooled connection reuse. Resolve/vet all answers; normalize mapped IPv4, reject private/special IPv4 and conservative IPv6 ranges; dial approved numeric IP directly while preserving original TLS SNI/Host and certificate validation. Response headers, read bytes (4 KiB discarded), concurrency, and time are bounded. Disable reuse so every dispatch/retry resolves again. One exact development http://localhost:<port>/webhooks exception pins to 127.0.0.1; production rejects it.

HMAC-SHA256 follows Standard Webhooks headers/exact-byte message format and matches its published Go vector. Receiver verifies bounded raw body, timestamp and signatures, then acknowledges/logs IDs only. It has no persistent business side effect or duplicate suppression yet. Active keys support overlap signatures; full receiver rotation migration tests remain milestone 6.

Basic finite retry scheduling is included to make new failure paths safe: 2xx delivered, configured transient status/network errors retry with full jitter and bounded Retry-After, permanent destination/TLS/HTTP failures dead-letter, ten reserved attempts and 24-hour cycle guard. Automatic expired-lease recovery and aged-pending sweeps remain milestone 5; they are not implicitly present because a lease column exists.

Worker requires the same external encryption key as API. Work deadline begins before claiming, completion has a separate five-second bound, and configuration requires lease > outbound timeout +6s. Shutdown stops claims, drains for grace, cancels remaining HTTP, then attempts bounded persistence. Root .env remains private/ignored; development URL exception stays blank by default.

All binary smoke targets now create/drop disposable migrated schemas in this project's local Compose DB, avoiding accidental dispatch of unrelated development work. The two-worker test uses one slot each and a local barrier proxy to require both processes to participate, then forwards exact signatures/bytes to the real compiled receiver. Integration tests separately use the disposable integration DB and isolated schemas.

## Commands actually run and outcomes

Earlier milestone race/integration/CLI/API checks passed, including exact-byte idempotency, rollback, tenant constraints, immutable snapshots, expiry and lifecycle locking.

Milestone 4 on 2026-10-15 (Darwin arm64):

- Verified primary Standard Webhooks specification/test vector, IANA IPv4/IPv6 special registries, and Microsoft's Azure virtual platform address documentation. References are linked in workers.md/ADR. No benchmark or deployment claims derived from these sources.
- make dev-up and make migrate-up: PostgreSQL healthy and attempt migration applied.
- make test-race: all unit packages passed, including signing/vector/timestamp/tamper/overlap, destination policy, real local TLS transport/SNI/DNS rebinding/redirect/proxy tests, 4 KiB response bound, finite retry/Retry-After bounds, worker slot capacity and grace cancellation, receiver signature validation, and previous suites. An initial generated signing-literal compile issue was corrected before passing checks.
- make lint: gofmt check and go vet passed. make build built API, worker, migration, admin, and receiver binaries.
- PostgreSQL integration tests passed with race detector: baseline eleven tests plus worker contention/fencing, post-commit network/tenant safety, disable/delete ordering, expiration-after-lock-wait, and retry/permanent failure recording. Final full count/results are recorded below.
- Eight concurrent claimers reserved twenty unique deliveries once each, with committed started attempts. Wrong/duplicate tokens failed, mismatched attempt identity rolled back completion, expired tokens failed, and a test-only simulated new lease fenced the old token. Attempt pages preserved old/new outcomes and excluded other tenants/private lease fields; cross-tenant attempt FK rejected.
- During actual local HTTP, receiver-side SQL successfully acquired endpoint and delivery row locks NOWAIT and saw the started attempt committed, proving claim locks/transaction ended before HTTP. Raw bytes and signatures verified. Tenant-ID substitution caused secret AAD failure before another HTTP request.
- Disabled/locked/deleted endpoints and disabled tenants were not claimed; disabled deferral consumed no attempt. Delete cancelled a reserved attempt and rejected late completion. Expiration while completion waited on a row lock was checked after acquiring it and rejected.
- Basic failure test durably scheduled a 500, preserved bytes/URL on attempt 2 (test explicitly made it due), then recorded a permanent 400/dead letter with both histories. Full timing/crash experiments remain next milestone.
- make test-worker-smoke: two compiled worker processes delivered twelve immutable signed payloads through a gated local proxy to the compiled receiver, all 204, one success attempt each, exactly two persisted worker IDs, no duplicate observed requests, bounded shutdown, and no payload/token/master-key/signing-secret values in captured process logs.
- make test-smoke, test-accounts-smoke, test-ingestion-smoke passed with disposable schema isolation, preserving prior health/auth/ingestion/restart/cancellation behavior. The helper requires the known local Compose database/profile and permits only ordinary sslmode query configuration before adding its generated search_path.

Workspace-local Go/GOPATH/GOCACHE were used; system Go still is not installed on PATH. Docker-specific filesystem/network permissions were granted for local tests. No public receiver requests, global system installs, dependency changes, Git remote/commit, hosted deployment, or fabricated metrics were introduced. Final checks/cleanup are recorded below.

## Tests not run and why

Automatic lease-expiry recovery, aged pending/deferred sweeps, abrupt worker-kill/ambiguous-success recovery experiments, receiver transactional duplicate suppression/replay, full old/both/new/retired receiver rotation scenarios, distributed rate limits/fairness, retention jobs, telemetry, CI/containers/deployment/egress enforcement/backups/benchmarks remain later milestones. Old-token tests explicitly simulate a recovery transaction in isolated test SQL; they do not prove a running recovery loop. The sample receiver acknowledges signatures without a business side effect.

## Known bugs / unresolved risks

No known milestone 4 acceptance blocker. A crashed worker can leave durable expired in_flight work until milestone 5 adds automatic recovery. Aged pending work waits for the upcoming sweeper. Acknowledgment does not mean exactly-once effects or downstream completion. No strict scheduling fairness/order guarantee. Network policy deliberately rejects some globally reachable special address assignments and must be maintained; deployment egress controls remain required. Master-key rotation tooling is not implemented. Source is local/uncommitted with no remote; image tags are version-pinned rather than digest-pinned. Do not deploy publicly yet.

## Next small task

Milestone 5: expired-lease recovery and aged-work sweeping under explicit locks, finite budget/deadline handling, crash failpoints confined to tests, outage/restart and acknowledgment-before-completion failure experiments, stale completion/duplicate behavior, and abrupt/graceful shutdown recovery bounds. Reuse current classifier/backoff foundation and retain immutable event/delivery identity/body/URL. Receiver business deduplication and replay belong to milestone 6.

## Questions I should be able to answer

- Why must claim commit before HTTP, and how did the test prove the locks were gone?
- Why does an unexpired token fence completion but still permit duplicate remote requests after failures?
- Why must expiration be checked after acquiring a contended row lock?
- Why are URL validation and connection-time DNS/IP vetting separate?
- How do direct-IP dialing and disabled connection reuse prevent a second DNS lookup/rebinding bypass?
- Why is a signed 2xx acknowledgment different from a single business effect?

## Final milestone 4 checks

Final full PostgreSQL integration race run passed all 16 tests after the post-lock expiry/budget changes. Final formatting/vet/build and all four isolated binary smoke targets passed. OpenAPI 0.4 JSON/internal references, documentation links, Python syntax, unchanged original specification, .env mode 0600, secret exclusion from source, ignored binary/cache paths, and whitespace checks passed. Smoke schemas and child processes were cleaned up by their helpers. make dev-down removed only RelayForge containers/network; the development volume remains.
