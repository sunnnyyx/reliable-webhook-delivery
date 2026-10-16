# Signed delivery workers

Milestone 8 runs separate API and worker processes, stores started attempt history with fenced leases, delivers signed immutable payloads, and records receiver acknowledgments. The example receiver verifies signatures, then atomically commits its processed-event marker and demo business record before returning 204. See [replay and receiver correctness](replay-and-receiver.md).

## Quick local demonstration

Use the existing development setup and preserve ENCRYPTION_KEY. The worker now also requires that key to decrypt endpoint signing secrets.

```sh
make dev-up
make migrate-up
make test-worker-smoke
```

The smoke target builds six binaries, creates disposable sender and receiver schemas in this project's development PostgreSQL, and runs the compiled API, two compiled workers, and compiled receiver. A local test proxy gates the first two requests until both workers participate; each worker has one slot. It forwards unchanged bytes/signature headers to the actual receiver, compares observed payload hashes with stored event hashes, and requires 12 deliveries with one successful attempt each, two distinct persisted worker IDs, and no secrets/payload markers in logs. It restarts the compiled receiver, resends all twelve requests, and verifies the business-record count remains twelve. It stops all child processes and drops both schemas. No public webhook is contacted and unrelated development work is not dispatched.

All binary smoke targets now use this isolated-schema helper. They require the project's local Compose database, unscoped DATABASE_URL for relayforge_dev, PROFILE=development, matching POSTGRES_HOST_PORT, and Docker access. Go integration tests separately use INTEGRATION_DATABASE_URL and isolated schemas.

## Manual receiver workflow

In ignored .env, explicitly set DEV_RECEIVER_URL=http://localhost:8090/webhooks. This permits only that exact development URL and pins its connection to 127.0.0.1:8090. Production rejects the setting. Other paths/ports, arbitrary private IPs, and broad private-network bypasses remain forbidden. The default is blank, with no exception.

Start make run-api, provision a tenant, and register an endpoint using that exact URL (see accounts-and-endpoints.md). Save its one-time signing_secret as RECEIVER_SECRETS in ignored .env. Also configure RECEIVER_DATABASE_URL for a receiver-owned database/schema and a stable RECEIVER_SOURCE for this sender tenant/source. Apply make migrate-receiver-up before starting it; [receiver setup](replay-and-receiver.md) has the complete workflow. Then run:

```sh
# separate terminal
make run-receiver
# another terminal
make run-worker
```

Publish an event with the endpoint's subscription type. Query GET /v1/deliveries?event_id=<event-uuid>, then GET /v1/deliveries/<delivery-uuid>/attempts using the tenant API key. A healthy local receiver produces delivered, HTTP 204, and one success attempt. GET /health/live on the receiver confirms the process is running; worker readiness and metrics default to private 127.0.0.1:8081; the address must remain a literal loopback address. A second worker needs a different WORKER_ADDR and must share the encryption key/database/URL policy.

The receiver supports one or two comma-separated whsec_ secrets via RECEIVER_SECRETS and binds only loopback. It verifies a bounded raw JSON envelope, delivery-ID header, timestamp within five minutes (past or future), and HMAC signatures before acknowledging. It logs event/delivery IDs without payloads or credentials. It deduplicates by configured source namespace and stable event ID using receiver-owned PostgreSQL tables. A changed body under an existing event ID returns 409. Already-processed valid requests return 204; signature verification still runs first. Receiver readiness checks its database/schema.

## Configuration

| Setting | Default / behavior |
|---|---|
| WORKER_CONCURRENCY | 8 slots per process, 1–128 |
| QUEUE_POLL_INTERVAL | 500ms; empty/error polls jitter over 75–125% |
| OUTBOUND_TIMEOUT | 10s, allowed 100ms–30s |
| DELIVERY_LEASE | 30s, maximum 2m; must exceed outbound timeout plus 6s |
| SHUTDOWN_TIMEOUT | 15s active-work grace, then cancellation |
| DEV_RECEIVER_URL | blank; development-only exact http://localhost:<port>/webhooks |
| ENCRYPTION_KEY | same external master key as API, required by worker |
| RECEIVER_ADDR | 127.0.0.1:8090, loopback only |
| RECEIVER_SECRETS | one or two comma-separated encoded signing secrets |
| RECEIVER_DATABASE_URL | required; receiver-owned PostgreSQL database/schema |
| RATE_LIMIT_ENABLED | false by default; explicit true requires REDIS_URL |
| TENANT_RATE_PER_SECOND / TENANT_BURST | 10 / 20, shared tenant bucket |
| MAX_TENANT_IN_FLIGHT / MAX_TENANT_BACKLOG | 4 / 10000, shared PostgreSQL limits |
| RECEIVER_SOURCE | required stable namespace, 1–128 letters/digits/dot/underscore/hyphen, starting with a letter/digit |

Each slot reserves at most one delivery that it can start immediately. No claimed batch waits behind a busy local queue. A work deadline begins before claiming, so claim/preparation time reduces remaining network time. Database claim/completion operations have five-second bounds. Lease validation reserves a persistence margin beyond network duration.

## Claim and completion transactions

Claim locks the least-recently-selected eligible tenant scheduler row, checks the tenant lifecycle under a shared row lock, then locks an active endpoint and due pending/retry-scheduled delivery using SKIP LOCKED. The shared per-tenant in-flight quota defaults to four. It checks the endpoint and tenant lifecycle and remaining attempt/cycle budget, snapshots stored event bytes and active encrypted endpoint keys, sets a new UUID lease token/expiry, increments cycle/lifetime counters, and inserts a started attempt atomically. If Redis admission is enabled, its Lua decision is bounded to 100ms inside this short transaction before creating any attempt. A denied/unavailable permission persists a tenant cooldown and delivery next-attempt time without a lease, attempt row, or budget increment. It commits before secret decryption or webhook DNS/TCP/TLS/HTTP. Redis itself is a trusted configured infrastructure dependency; this bounded admission call is the only intended network call inside claim. See [rate limits and tradeoffs](rate-limits.md). Claiming before a crash still reserves/consumes an attempt even if no request was sent.

Tenant-before-endpoint ordering for admission/claim is compatible with endpoint-before-delivery deletion/recovery. Claim skips locked/disabled/deleted endpoints and disabled tenants without creating an attempt. Scheduler locking serializes admission briefly per tenant; concurrent network sends remain possible after commit. Least-recently-selected tenants and the shared in-flight quota provide approximate fairness. Strict fairness, endpoint round robin, and ordering are not guaranteed.

Completion locks the delivery first, then checks its tenant, in_flight state, token, and expiration using the database clock after any lock wait. It updates delivery and attempt together. Wrong, expired, deleted, already completed, or superseded leases cannot overwrite current state. Attempt-update failure rolls back the delivery update too. Tokens fence database state, not the receiver: a network request already authorized may finish after endpoint disable/delete. Delete atomically cancels unfinished delivery rows and started attempts; old results are rejected.

Attempt views are tenant-authorized and paginated by cycle_number then attempt_number ascending, default 25/max 100. Cursors bind tenant and delivery. They expose worker ID, timestamps, outcome, HTTP status, latency, and error category; they omit lease tokens and signing material. cancelled is an additional explicit outcome for endpoint deletion.

## Outbound destination protection

Production requires HTTPS/443 DNS names with no credentials/fragments/IP literals. A dedicated transport does not inherit environment proxies, follows no redirects, retains certificate validation and original-host TLS SNI, bounds headers/timeouts/concurrency, and reads/discards at most 4 KiB of response body. It forwards no API authorization, trace baggage, or caller-supplied headers.

At each new connection, resolve all DNS answers and reject the whole result if any address is forbidden. Normalize IPv4-mapped addresses before checking. Dial an approved numeric address directly, rather than resolving the hostname again. Keep-alive and HTTP/2 reuse are disabled so each request revalidates DNS, including retries; this trades connection overhead for a simple auditable first implementation.

The policy conservatively excludes special-purpose IPv4 ranges (including loopback/private/link-local/shared/reserved/documentation/benchmark/multicast), [Azure's virtual platform address](https://learn.microsoft.com/en-us/azure/virtual-network/what-is-ip-address-168-63-129-16), and special IPv6 ranges. IPv6 is limited to ordinary global-unicast 2000::/3 excluding special assignments; translation/tunneling prefixes are blocked. Even some special ranges marked globally reachable are intentionally excluded. Maintain this conservative snapshot against [IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry) and [IANA IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry); deployment egress controls remain a required second layer later.

Tests cover mixed DNS results, public-to-private answer changes, mapped/private IPv6, direct numeric dialing, TLS SNI/Host preservation, bad certificates, disabled proxy inheritance, blocked redirects, and bounded response consumption. Fake resolvers/dialers route only test traffic to local TLS servers; production has no resolver/TLS bypass switch.

## Signing

webhook-id is the stable delivery UUID. webhook-timestamp is fresh Unix seconds per send. webhook-signature contains one or two space-separated v1 signatures over delivery-ID.timestamp.exact-stored-body. Secrets are decoded before HMAC-SHA256; active keys are loaded when authorizing each dispatch. The signer matches the [published Standard Webhooks Go vector](https://github.com/standard-webhooks/standard-webhooks/blob/main/libraries/go/webhook_test.go), with protocol details in the [specification](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md). Rotation tests cover old-only, both, and new-only receivers during overlap, queued work loading current keys, and new-only signing after retirement. A new-only receiver rejects old signatures.

## Failure handling boundary

A completed 2xx attempt marks delivered. Other 4xx/3xx and invalid destinations/TLS validation fail permanently. Network/timeouts, 408/425/429/5xx schedule bounded retries. Basic full-jitter/Retry-After scheduling and finite ten-attempt/24-hour guards are included now so new failure paths do not loop unboundedly; failure experiments cover outages, worker restart, and process kills.

Each worker runs maintenance immediately at startup and every five seconds after a pass. A pass makes at most 100 expired-lease transitions and 100 aged-queue transitions, each in its own transaction, under a five-second context. Endpoint-before-delivery locks with SKIP LOCKED coordinate maintenance, claims, and endpoint deletion. Selection and recovery planning use the PostgreSQL clock. Expired attempts finish as lease_expired; the delivery clears its lease and either schedules full-jitter retry or dead-letters at the ten-attempt/24-hour limit. Counts are never refunded. If the attempt invariant fails, the entire transition rolls back.

The separate queued-work pass expires pending/retry_scheduled rows even when disabled, future-due, or deferred; it consumes no attempts. Terminal rows remain terminal. Multiple workers may scan concurrently. The new partial age index supplements the existing lease-expiry index. Apply migrations before starting workers.

With an available database, no lock contention, and a small backlog, a crashed lease becomes eligible after expiry, then waits for a maintenance pass (normally up to five seconds), jittered backoff (up to two seconds after attempt one), and a worker poll (default up to 625 ms). Default lease duration is 30 seconds. These are scheduling components, not a guaranteed end-to-end bound: batch saturation, locked endpoints, database outages, receiver availability, and later exponential backoff can extend recovery. Maintenance failures log bounded categories and retry next pass. No automatic retry is promised while PostgreSQL is unavailable.

On shutdown, stop claims, let active slots drain within grace, then cancel remaining HTTP. Completion persistence has an additional five-second bound after cancellation. Abandoned/expired leases remain durable for automatic recovery by a running or restarted worker. A 2xx confirms receiver acknowledgment, not downstream business completion or exactly-once effects.


## Failure verification

The integration suite kills real test worker processes after committed claim and after a real local HTTP receiver committed its effect and returned 204, before local completion. Test-only dispatch decorators expose synchronization gates; production binaries have no crash switches. A 300 ms test lease accelerates expiry, and tests wait for real expiry and jitter eligibility without changing those timestamps. Recovery preserves identity/body, records abandoned attempts, and retries; acknowledgment-before-kill produces two receiver effects, in the intentionally non-deduplicating milestone 5 fixture. A separate milestone 6 process-kill test uses the real receiver/production signer and proves two requests leave one local business record.

A separate real HTTP test responds 503, 503, 429 with Retry-After: 1, then 204. A new worker runner/Store starts after each persisted attempt; natural due times and the minimum Retry-After interval are checked. This is an in-process runner restart experiment; abrupt OS process loss is covered separately. Concurrent scanners, stale completion, bounded batches, disabled/future-due age sweeps, ten-attempt exhaustion, and rollback on attempt invariant failure are also exercised. Database outage recovery during an active transaction and infrastructure-level failover are not covered by these tests.


## Metrics and linked traces

Every worker exposes bounded OpenTelemetry metrics, including dispatch outcomes/status classes and duration, first-attempt queue delay, committed/stale/error completion, replay/recovery/deferral counters, and critical dependency availability. Global durable queue snapshots are exported by API operational listeners, not summed across workers. Worker operational addresses are loopback-only. See [metric definitions](observability.md).

Optional OTLP/HTTP export starts a new dispatch trace linked to the stored original acceptance span; completion is a child of dispatch. Trace IDs/correlation rows live outside immutable outgoing bytes. Repository/API/worker logs correlate request IDs and object IDs with trace/span IDs where present. Incoming public traceparent/tracestate/baggage are not trusted/extracted, and receiver requests keep only the existing allowlisted signing/content headers. Collector errors do not change delivery readiness/state; export is sampled/best effort. Shutdown drains active work, then allows up to three seconds for telemetry shutdown.
