# Rate limits, fairness, and backlog admission

## Enable the optional limiter

```sh
make dev-rate-up
make migrate-up
```

Set the following in ignored .env for a local distributed-admission demo:

```dotenv
RATE_LIMIT_ENABLED=true
REDIS_URL=redis://127.0.0.1:16379/0
RATE_LIMIT_PREFIX=relayforge-v1
TENANT_RATE_PER_SECOND=10
TENANT_BURST=20
MAX_TENANT_IN_FLIGHT=4
MAX_TENANT_BACKLOG=10000
```

Adjust the Redis port to REDIS_HOST_PORT if needed. The optional Compose profile pins redis:8.10.2-alpine, binds loopback only, disables persistence, and uses 64 MiB/noeviction. The standard dev-up starts only PostgreSQL. dev-down stops both project services and preserves the PostgreSQL volume. Redis stores permissions only; restarting it can reset a tenant to a fresh burst but cannot delete accepted delivery work. Production Redis needs private networking, authentication/ACLs appropriate to the script, TLS via rediss, and deliberate availability/persistence policy. Those production services/roles are not provisioned here.

RATE_LIMIT_ENABLED defaults to false. Disabled mode deliberately bypasses Redis admission and is logged at worker startup; shared PostgreSQL tenant quotas and backlog admission remain active. A true setting with missing/invalid URL fails configuration. An enabled limiter never silently switches to disabled mode when Redis is unavailable. All worker replicas must use the same enabled mode, Redis service/database, prefix, rate/burst, and quota settings. API/replay replicas must agree on the backlog cap. Mixed configuration can weaken deployment-wide policy; deploy changes consistently. The Lua script rejects conflicting rate/burst values for an existing bucket instead of issuing another burst.

Defaults/ranges: rate 10/s and burst 20 (each integer 1–10000); tenant in-flight quota 4 (1–128); live backlog cap 10000 (1–1000000); prefix 1–64 ASCII letters/digits/underscore/hyphen. Prefix separates independent sender deployments, not individual workers. A changed prefix or cleared Redis database resets rate state; do not use that as an outage workaround.

## Atomic permission and clocks

One Lua token bucket is keyed by prefix + tenant UUID. It uses Redis TIME in milliseconds, fractional continuous refill, a capacity cap, and one token per grant. This is admission to dispatch, not delivery success or a promise about exact packet-arrival timing. The implementation follows the atomic token-bucket approach described in [Redis's Go guide](https://redis.io/docs/latest/develop/use-cases/rate-limiter/go/), with server-side time, explicit idle TTL, and conservative errors added here.

An idle bucket expires after twice its full-refill duration, at least one second; restarting after that much idle time correctly permits a full burst. Denials update the TTL and return the next token delay. Backward clock steps never move the last-refill time backward and delay eligibility conservatively. Forward steps refill at most one burst. The guarantee assumes one correctly configured standalone Redis clock/state; failover, state loss/eviction, and configuration resets can grant a fresh burst. This is not a durable strict rate ledger or a tested Redis Cluster/failover implementation.

Each Redis admission operation has a 100 ms context/socket/pool bound, with automatic command retries disabled. A lost reply might have consumed permission, so the worker fails closed instead of trying the operation again. A later PostgreSQL rollback or worker crash can also waste a granted token. Tokens are not refunded; conservative throughput loss is safer than granting unconfirmed permission twice.

## Claim and durable deferral

Claim locks scheduler -> tenant (shared lifecycle lock) -> endpoint -> delivery. Scheduler locking serializes one tenant's admission across workers and lets fresh statements recheck its global in-flight count. The endpoint/delivery locks prevent lifecycle mutation during authorization. The optional Redis operation is deliberately the only bounded infrastructure network call inside this claim transaction, before attempt/lease creation. It can add up to its 100 ms budget to these locks. Secret decryption and webhook DNS/TCP/TLS/HTTP still occur after claim commit. No receiver HTTP runs inside the transaction.

On denial, the selected delivery's next_attempt_at and error category are persisted, and a shared tenant dispatch_after pauses all its other queued deliveries too. This prevents a burst of one Redis denial per row. Delay is the token wait, with a 25 ms minimum, one-hour maximum, and up to 50 ms additional jitter. An error uses one second plus up to 50 ms jitter and category limiter_unavailable. The delivery retains pending/retry_scheduled state. No lease, attempt row, rate_limited attempt outcome, or budget increment is created for a local deferral. Prior attempt history remains. Last HTTP status remains the last real HTTP result; last_error_code may describe the later local deferral.

A running/restarted worker retries after stored eligibility and ordinary polling. Age recovery does not require Redis and still dead-letters expired deferred work. A successful permission is followed by a fresh cycle-eligibility check before reservation. Normal claims create started attempts only when allowed, and commit before sending. Shutdown/cancellation and database errors roll back incomplete decisions; a Redis permission may still be conservatively spent.

## Fairness and quotas

PostgreSQL tenant_dispatch_state rows order eligible tenants by least-recent selection; FOR UPDATE SKIP LOCKED avoids busy schedulers. A tenant's quota counts global in_flight rows, including expired leases until recovery finalizes them. Fresh count under the scheduler lock prevents multiple workers overshooting a consistently configured quota. Locked tenants/endpoints/deliveries yield their selection position without an attempt; workers then use their normal empty-poll backoff.

A large tenant can occupy at most its configured quota of concurrent leases, leaving other slots for another eligible tenant when there are more total slots than the quota. This can intentionally leave capacity idle when only one tenant has work. In a tested workload, two two-slot worker runners held two slow requests for a 100-event tenant while completing another tenant's two-event queue. This demonstrates approximate fairness for that workload, not a strict scheduling/latency SLA. There is no preemption, endpoint round robin, weighted fairness, or guaranteed service under a single slow slot or indefinitely locked rows.

## Backlog and lifecycle

New ingestion/replay serializes on its tenant row using FOR NO KEY UPDATE before endpoint/delivery locks. It counts live pending/retry_scheduled/in_flight deliveries and admits only the complete proposed fan-out, or one replay slot. The row lock is compatible with ordinary FK key-share checks; it avoids concurrent admission overshooting the same cap. Terminal transitions can only free capacity, so an overlapping completion may cause a conservative temporary rejection, not oversubscription. Counts use partial PostgreSQL indexes; no load/scale performance claim is made.

Full capacity returns 429 backlog_limit and Retry-After: 1; the transaction leaves no partial event/fan-out, replay cycle, or new receipt. Retry the same idempotency key once capacity frees. Matching prior ingestion/replay receipts are returned before the capacity check. Events with no matching endpoints need no delivery slots. This is a delivery-backlog cap, not a general ingestion request limit or a cap on terminal event/history storage. Retention cleanup is still later work.

Tenant lifecycle is checked under a shared tenant lock during claim and admission under its tenant write lock. A tenant disable already in progress prevents new authorization, even if an earlier MVCC snapshot saw it active. Endpoint mutations use endpoint locks. Disable holds future claims and consumes no attempts; re-enable resumes eligible work. An HTTP request authorized before disable may still return and complete. Delete cancels unfinished rows/started attempts and rejects late completion, even if the receiver later returns 204. None of these operations can recall a request already sent. Recovery/delete need endpoint -> delivery; completion only needs delivery -> attempt, so they do not acquire parent locks in reverse order.

## Outages and operational surface

Configured Redis outage prevents new outbound attempts and makes worker /health/ready return 503 limiter_unavailable. /health/live remains 200. API readiness still probes PostgreSQL only, so durable acceptance can continue up to the backlog cap. Readiness probes Redis connectivity, not every possible ACL/configuration mismatch; script errors also appear in counters and persisted deferral categories. Health endpoints never return Redis URLs or raw errors.

The worker operational listener (default 127.0.0.1:8081) always exposes /metrics; enabled Redis admission additionally records: relayforge_rate_limit_permissions_total, relayforge_rate_limit_denials_total, and relayforge_rate_limit_errors_total. They are process-local monotonic admission counters, reset on restart, with no tenant/URL labels. Permissions are not a count of successful network sends; SQL rollback can consume a grant. The application now enforces literal loopback binding. Keep this listener private via loopback/firewall; it has no tenant bearer-auth route and must not be published with the API. Broader metrics, traces, dashboards and gauges are implemented in milestone 8; see [observability](observability.md).

Fail-closed behavior trades dispatch availability for rate safety. Already-authorized requests can finish during an outage. If disabling admission is an intentional operational policy change, configure all replicas consistently; outage handling itself never does so automatically. Restored Redis permits the same running workers to resume after persistent cooldowns. PostgreSQL continues owning all events, deliveries, attempts, replay receipts, and recovery history.

## Verification

```sh
make dev-rate-up
# Explicit disposable PostgreSQL URL, as documented in setup.md:
# INTEGRATION_DATABASE_URL=postgres://.../relayforge_migration_test?sslmode=disable
# INTEGRATION_REDIS_URL=redis://127.0.0.1:16379/15
make test-integration
make test-rate-limit-smoke
```

Integration tests use unique Redis key prefixes and disposable PostgreSQL schemas; they never FLUSH a shared Redis database. They verify atomic burst/refill/TTL across independent clients, configuration mismatch, durable no-attempt denials/outages, recovery age sweep during outage, quota/fairness, concurrent backlog admission/rollback/idempotency, replay capacity, endpoint mutation during gated real HTTP, and tenant-disable lock ordering. Earlier claim/fencing/replay/crash/rotation tests remain part of the suite.

The compiled smoke uses two one-slot worker processes and a test-only TCP proxy to one real Redis instance. It starts with Redis unreachable, confirms durable zero-attempt deferrals and degraded readiness, restores connectivity, gates two signed requests to prove both processes participate in one rate=1/burst=1 bucket, cuts established Redis connections, verifies pending work still has no attempts, and restores connectivity again. The same workers finish four signed deliveries with four receiver business records; counters/log exclusions/shutdown are checked. This is a controlled local network outage, not a real Redis crash/failover/persistence-loss benchmark.

Versions added in this milestone: [go-redis v9.23.0](https://github.com/redis/go-redis/releases/tag/v9.23.0), [Redis 8.10.2](https://github.com/redis/redis/releases/tag/8.10.2). Docker image tags are pinned by version, not digest. No production deployment or throughput/latency benchmark is claimed.
