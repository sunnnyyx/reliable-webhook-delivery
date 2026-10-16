# 0009 — Shared tenant admission and conservative Redis permissions

Status: accepted, milestone 7.

Implementation update: milestone 8 now provides broader private metrics, traces, live dashboard queries and an operator runbook; see [observability](../observability.md). The decision below records the earlier stage.

Use optional Redis Lua token buckets keyed by deployment prefix/tenant, with Redis server time and idle TTL. Pin go-redis v9.23.0 and Redis 8.10.2 after checking their official releases. [Redis's token-bucket guide](https://redis.io/docs/latest/develop/use-cases/rate-limiter/go/) supports the atomic-script approach; our implementation adds server time, bounds, expiry, and conservative clock/error handling. Redis is never durable queue storage.

Acquire bounded permission under scheduler -> tenant -> endpoint -> delivery locks, before attempt creation. A 100 ms Redis context bounds this infrastructure call inside claim; webhook HTTP and secret decryption remain after commit. Denials/errors persist a tenant cooldown and delivery next eligibility without a lease/attempt/counter increment. Disabled mode is explicit; configured errors fail closed. Do not retry an ambiguous Redis command or refund a grant lost to SQL rollback/crash.

Store tenant selection order in PostgreSQL, lock scheduler rows with SKIP LOCKED, and enforce a shared in-flight quota under that lock. Least-recent selection plus bounded tenant concurrency provides approximate fairness, not preemption or a strict service guarantee. Locked lifecycle rows yield selection position rather than monopolizing every poll.

Serialize new ingestion/replay admission on the tenant row (FOR NO KEY UPDATE) before endpoint/delivery locks, then count indexed live rows and admit the entire fan-out/replay or reject with 429. Existing idempotency receipts bypass capacity checks. Shared lifecycle locks fence new tenant authorization against disable. Endpoint delete/disable semantics for already-authorized HTTP remain explicit and tested.

Readiness degrades for configured Redis connectivity loss; API PostgreSQL acceptance stays independent. Expose only bounded process-local admission counters on the private worker operational listener. Full telemetry is next. See [rate limits](../rate-limits.md) for TTL/clock assumptions, mixed-configuration/state-loss limits, outage experiment, and backlog scope.
