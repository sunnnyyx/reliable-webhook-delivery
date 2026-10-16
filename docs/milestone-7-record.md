# Historical milestone 7 record

## Current milestone

7 — Rate limits and lifecycle races: complete. Await the user's continuation before milestone 8.

The specification has 12 milestones numbered 0–11, including milestone 9's optional product interface and milestone 11's deployment/portfolio work. After milestone 7, milestones 8–11 remain. The initial conversational count of 11 was corrected after inspecting the milestone 11 heading.

## Completed work

Milestones 0–6 established PostgreSQL-backed API/auth/endpoints, exact-byte idempotent ingestion, immutable fan-out snapshots, encrypted signing secrets, fenced worker attempts/completion, protected outbound HTTP, finite retries/recovery, idempotent replay with history, transactional receiver deduplication, and signing-key rotation. See the [milestone 6 record](milestone-6-record.md) for prior evidence and links to earlier historical records.

Milestone 7 adds optional Redis Lua token buckets per deployment/tenant. Redis TIME controls refill; idle TTL bounds ephemeral bucket lifetime; grants/denials are atomic across clients. Admission is bounded to 100 ms under the short claim transaction before any attempt/lease/counter increment. Denials/errors persist tenant-wide cooldown and delivery scheduling. Errors fail closed, with no automatic command retry or token refund after uncertain outcomes. Redis never owns delivery work. Enabled mode requires explicit valid configuration; production Redis URLs require TLS. Disabled mode is explicit/logged and remains the default.

PostgreSQL tenant scheduler rows implement least-recent selection and shared per-tenant in-flight quotas (default four). Tenant/endpoint/delivery locks recheck lifecycle and serialize cross-worker authorization; locked rows yield selection order. Backlog admission serializes new ingestion/replay per tenant and counts live work (default 10000). Full fan-out or one replay must fit; otherwise 429 backlog_limit/Retry-After: 1 rolls back the entire request. Existing idempotency receipts and no-target events retain their behavior. Shared PostgreSQL controls apply even without Redis.

Worker readiness checks configured Redis connectivity; API readiness remains PostgreSQL-only. Basic bounded admission counters appear on the private worker operational listener with no tenant/URL labels. Full telemetry, traces/dashboards, and retention operations remain later work.

## Changed files

internal/ratelimit/redis.go; migrations/00007_scheduling.sql; Store scheduling fields; internal/repository/postgres/{claim,admission,ingestion,replay}.go; API backlog mapping and optional health dependency; configuration/production wiring and configuration/health tests; tests/integration/scheduling_test.go; older contention fixture raises its test-only quota to preserve its intentional twenty outstanding leases; deploy/compose.yaml optional Redis profile; Makefile; scripts/smoke-rate-limit.py; go.mod/go.sum; .env.example; api/openapi.json; README and current guides; docs/rate-limits.md; ADR 0009; this log and the historical milestone 6 record. Original specification and ignored local secrets remain unchanged.

Dependencies added: go-redis v9.23.0 and its pinned indirect modules. Redis image redis:8.10.2-alpine was pulled and actual server reported 8.10.2. Official references and clock/TTL/state-loss/mixed-configuration assumptions are linked in [rate limits](rate-limits.md). PostgreSQL/Go/pgx/Goose pins are unchanged. Image versions are pinned by tag rather than digest.

## Executed evidence

- Independent Redis clients contending on one bucket admitted exactly the configured initial three-token burst; remaining calls denied with a bounded wait. Natural server-clock refill, TTL, conflicting rate/burst configuration, and malformed token state were checked. Prefixes are unique per test; no FLUSH operation touches shared Redis.
- Denial across fresh worker repository/client instances persists cooldown/next eligibility without an extra attempt or counter increment. Work resumes naturally as permission refills. An unavailable dependency persists limiter_unavailable with zero attempts, and PostgreSQL age recovery still dead-letters deferred work while Redis is unavailable.
- Two two-slot worker runners use a shared two-lease tenant quota. A 100-event tenant occupies two deliberately gated real HTTP requests while a two-event tenant finishes. Exactly two large-tenant attempts exist while its quota is occupied, demonstrating bounded monopolization for this workload, not a strict fairness/latency guarantee.
- Twelve concurrent two-target ingestion requests at capacity two admit one whole fan-out and one receipt. Rejections leave no partial reservations/events. Matching acceptance still returns when full. A single replay fits one released slot; a full backlog rejects ingestion/replay with API 429 and retry hint. Zero-target events consume no delivery capacity.
- Real HTTP gates prove disable blocks future claims while an authorized request can finish and complete. Re-enabling resumes the other delivery without consumed attempts. Delete cancels unfinished work/started attempts and rejects a late 204 completion.
- Tenant disable is held uncommitted under an observed lock: claim cannot authorize from its earlier active MVCC view; new ingestion waits for admission, sees disabled after acquiring the lock, and rolls back its provisional receipt/event. No reverse-lock deadlock occurred in the tested interleavings.
- Two compiled worker processes share one real rate=1/burst=1 bucket through a test-only Redis TCP proxy. Startup outage leaves all four deliveries pending with zero attempts and 503 worker readiness while liveness/API readiness stay healthy. Restored Redis permits two gated signed sends, proving both worker IDs participated; the gate then cuts existing Redis connections. Remaining work stays attempt-free during the runtime outage. Restoring connectivity lets the same workers finish all four deliveries with four receiver business records, one success attempt each, bounded counters, graceful shutdown, and no payload/credential values in process logs.
- Existing health/auth/ingestion/two-worker signed delivery/receiver restart/replay smoke checks continue passing. Earlier fencing/recovery/ambiguous-success/deduplication/rotation integration tests remain in the full race suite.

## Corrections during implementation

An initial yield implementation added a short persistent cooldown for locked endpoints, causing the existing immediate re-enable test to fail. Yield now updates only selection order; ordinary worker polling handles the empty result. Rate-denial/outage cooldowns still persist as intended. A backlog test initially disabled the same endpoint it intended to replay; the fixture now disables the other endpoint so the test isolates capacity rather than endpoint availability. Redis bucket validation fails closed on malformed recorded token state instead of interpreting it as a fresh burst. The test-only twenty-lease contention fixture raises its quota; production default remains four.

## Limits and next work

Rate limiting is optional and off unless explicitly enabled. Consistent replica configuration is required. Redis grants are conservative permissions, not successful sends; ambiguous replies/SQL rollback/crashes can waste tokens. Idle expiry is safe after refill time, but Redis state loss/eviction/changed namespace can grant a fresh burst. The local Redis service uses no persistence and noeviction; real failover/persistence loss, Redis Cluster, production ACLs/auth/TLS deployment, and clock jumps under infrastructure failure are not tested.

The fairness test establishes behavior for its explicit 100:2 workload and free-slot/quota arrangement. There is no preemption, weighted/strict fair scheduling, endpoint fairness, or service SLA under a single slow slot/indefinitely locked rows. Backlog counts use indexed live rows and per-tenant admission locking; no scale/performance claim is made. The cap does not bound terminal history or no-target event volume. Retention cleanup, receiver production roles, PostgreSQL failover/restore, full telemetry, CI, deployment/egress controls, and benchmarks remain later/unverified work. Already-authorized HTTP can finish during disable/outage; no database fence can recall external effects.

Next: milestone 8 — operational metrics/traces, backlog/failure views, private telemetry, readiness/degraded behavior, dashboards, and an operator runbook. Milestone 9 is optional product UI; 10 is benchmarks/hardening; 11 is deployment/portfolio. No hosted deployment, remote Git operation, commit, or fabricated benchmark was introduced.

## Questions to explain

- Why obtain permission before reserving an attempt, and why can a granted token still be wasted?
- Why is a local denial a scheduling update rather than a network-attempt outcome?
- Why does a tenant-wide cooldown avoid one Redis call per queued row during outage?
- How do scheduler locks and fresh global in-flight counts prevent cross-worker quota overshoot?
- Why does backlog admission use FOR NO KEY UPDATE and preserve existing idempotency receipts?
- What exactly did the 100:2 fairness test establish, and what did it not establish?
- Why can API acceptance stay healthy while outbound worker readiness is degraded?


## Final milestone 7 checks

On 2026-10-15, final unit race tests passed, and the full PostgreSQL/Redis integration race suite passed all 36 substantive tests plus the gated helper entry. Final gofmt/vet passed. All five binaries built and all six binary smoke targets passed: health, accounts, ingestion, two-worker signed delivery/receiver restart, replay, and distributed rate-limit/outage recovery. Following the malformed-bucket validation change, the unit race suite, full integration race suite, and compiled rate-limit smoke passed again. Sender migration 7 applied to the development database; integration migration round trips passed.

OpenAPI 0.7 JSON/internal references, documentation links, Python syntax, unchanged original specification, the 12-milestone count, .env mode 0600, and private-value exclusion from source passed. A final database query found zero generated smoke schemas. Child processes/proxies were stopped by their helpers. dev-down with the rate-limit profile removed only this project's PostgreSQL/Redis containers/network and preserved the PostgreSQL volume. Redis permissions are intentionally ephemeral in this development profile. No ignored secret values were changed, and optional admission was not silently enabled in the user's existing .env. No production infrastructure, hosted deployment, remote Git operation, commit, benchmark, or system installation was introduced.
