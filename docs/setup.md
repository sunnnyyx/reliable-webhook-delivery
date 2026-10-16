# Local setup and current commands

Run all commands from the repository root. You need Go **1.27.2**, Git, Make, and a working Docker engine with Compose. Python 3 is required for the optional process smoke test. Install Go for your machine using [official instructions](https://go.dev/doc/install). This build uses pgx **v5.11.0**, Goose **v3.29.0**, and PostgreSQL image **18.6-alpine3.24**. go.mod pins Go and library versions; go.sum records dependency checksums once dependency resolution completes. Container tags pin release versions, not immutable image digests; production digest pinning is deferred to deployment.

## Development

```sh
cp .env.example .env
# Edit .env: replace the example password in POSTGRES_PASSWORD and DATABASE_URL.
# Generate and set ENCRYPTION_KEY (see docs/accounts-and-endpoints.md).
# Use URL encoding in DATABASE_URL if your password contains reserved characters.
make dev-up
make migrate-up
make build
make test
make test-race
make test-smoke
make lint
```

Expected: PostgreSQL becomes healthy, all pending schema migrations are applied, six binaries are built, tests pass, and vet/format checks complete. These are instructions, not a claim that database checks passed here.

In separate terminals:

```sh
make run-api
make run-worker
```

Verify:

```sh
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
curl -i http://127.0.0.1:8081/health/ready
```

Healthy API/worker routes return 200 JSON with status and request_id, plus X-Request-ID. Liveness does not call PostgreSQL. Readiness returns 503 with a sanitized storage_unavailable error if its bounded database ping fails. Startup requires a successful database ping within STARTUP_TIMEOUT; it fails without logging credentials. Ctrl-C or SIGTERM begins shutdown; SHUTDOWN_TIMEOUT bounds request draining, then open connections close. The worker now claims and sends eligible work; read [worker setup](workers.md) before running it against existing data. Both API and worker require the existing encryption key.

If port 5432 is already allocated, set POSTGRES_HOST_PORT to a free port (for example 15432) and update the port in DATABASE_URL to match. The default remains 5432.

Make loads and exports .env. Directly launching bin/api, bin/worker, or bin/migrate requires supplying the same variables through the environment. Worker/receiver configuration and the optional exact local URL exception are documented in [workers](workers.md).

SQL migrations are embedded in the migration binary; run migration-up separately before the API. There is no automatic schema migration at startup.

## Configuration

| Variable | Default / requirement |
|---|---|
| PROFILE | development or production; default development |
| DATABASE_URL | Required PostgreSQL URL with host and database |
| API_ADDR | :8080 |
| WORKER_ADDR | 127.0.0.1:8081; private health/metrics endpoint |
| DB_MAX_CONNS | 10 per process; range 1–100 |
| WORKER_CONCURRENCY | 8; range 1–128; active delivery setting |
| QUEUE_POLL_INTERVAL | 500ms; range 10ms–1m; active delivery setting |
| STARTUP_TIMEOUT | 5s; range 1ms–30s |
| READY_TIMEOUT | 1s; range 1ms–5s |
| SHUTDOWN_TIMEOUT | 15s; range 1ms–1m |
| ENCRYPTION_KEY | API/worker require base64 of 32 random bytes; preserve securely |

Invalid configuration exits nonzero. Each process has its own connection pool; account for all replicas when choosing PostgreSQL connection limits. The API and worker require ENCRYPTION_KEY; see [credential setup](accounts-and-endpoints.md). Never commit .env.

## Production profile

Set PROFILE=production and use a private PostgreSQL URL with sslmode=verify-full and trusted certificate configuration. The Compose file is development infrastructure; it exposes PostgreSQL only to local loopback and is not a production deployment. Production migration down is rejected. Tenant authentication is implemented in milestone 2. Connection-time DNS/IP validation is implemented in milestone 4. Public deployment, egress restrictions, TLS ingress, and production telemetry infrastructure are later milestones; this local project is not a hosted deployment.

## Disposable database migration test

With development PostgreSQL running:

```sh
docker compose --env-file .env -f deploy/compose.yaml exec postgres createdb -U relayforge relayforge_migration_test
# Set INTEGRATION_DATABASE_URL to the matching PostgreSQL URL for this disposable database.
make dev-rate-up
# Set INTEGRATION_REDIS_URL=redis://127.0.0.1:16379/15 (adjust REDIS_HOST_PORT if needed).
make test-integration
```

The integration test creates a unique schema, applies up twice, checks tenants exists, applies down, checks removal, and applies up again. It cleans up its schema. The test requires CREATE SCHEMA permission and fails if no URL is provided. It never uses DATABASE_URL implicitly. Do not point it at production. You may drop the explicitly disposable database afterwards.

make migrate-down rolls back one migration and is development-only. Future down migrations may destroy data; invoke them only on disposable development databases. make dev-down stops development containers while retaining the volume; no reset/delete-volume target exists.

## Implemented commands

- dev-up / dev-down: start PostgreSQL and wait for health / stop this project's containers, including optional Redis.
- dev-rate-up: start PostgreSQL plus the optional Redis rate-limit profile and wait for health.
- migrate-up / migrate-down: apply pending migrations / roll back the latest development migration.
- run-api / run-worker: launch the separate API and delivery-worker processes.
- build: build API, worker, migration, admin, receiver, and console binaries in ignored bin/.
- test / test-race: run unit tests / unit tests with race detector.
- test-integration: requires INTEGRATION_DATABASE_URL and INTEGRATION_REDIS_URL; run real PostgreSQL migration, ingestion, worker, recovery, replay, receiver, and rotation tests.
- test-smoke: build and launch API/worker on ephemeral loopback ports, verify health/request IDs/JSON logs, then check SIGTERM shutdown against the configured real database.
- fmt / lint: format Go / require formatting and run go vet.

- admin: private tenant/key operations; see [accounts](accounts-and-endpoints.md).
- test-accounts-smoke: exercise actual CLI/API auth, isolation, rotation, and log redaction with synthetic development tenants.

- test-ingestion-smoke: verify actual API restart durability, idempotency, event/delivery views, cancellation, and payload/secret log exclusion using synthetic development history.

See [ingestion](ingestion.md) for submission and listing commands.

- run-receiver: start the local receiver (requires RECEIVER_SECRETS, RECEIVER_DATABASE_URL, RECEIVER_SOURCE).
- migrate-receiver-up / migrate-receiver-down: apply receiver-owned migrations / roll back its latest migration (down requires explicit PROFILE=development).
- test-replay-smoke: compiled API/worker/receiver, concurrent same-key replay, preserved attempts/bytes, and receiver duplicate suppression.
- test-worker-smoke: two compiled worker processes and receiver, gated local proxy, immutable-byte/signature/attempt verification.

All binary smoke targets now require the local project Compose database and use disposable migrated schemas via scripts/smoke_db.py. They do not consume unrelated application work. Crash/recovery, replay, and receiver rotation experiments run in the integration suite. Seed/benchmark, production deployment and infrastructure failover remain planned. See [replay and receiver setup](replay-and-receiver.md).

## Distributed dispatch admission

See [rate limits](rate-limits.md). Redis is optional and explicitly enabled with RATE_LIMIT_ENABLED=true; invalid/missing REDIS_URL then rejects configuration. A configured outage fails closed for new outbound dispatch, while PostgreSQL acceptance/replay/recovery remain available within their own limits. Production Redis URLs require rediss TLS. API readiness does not depend on Redis. Worker readiness checks configured Redis; basic admission counters are served only on the worker's private operational listener.

make test-rate-limit-smoke requires make dev-rate-up. It runs two compiled workers with a shared rate=1/burst=1 bucket, a controlled Redis TCP proxy, and the signed receiver. Startup/runtime outages, zero-attempt deferrals, degraded/recovered readiness, delivery recovery without worker restart, bounded counters, and log exclusion are checked. The proxy and crash gates exist only in tests/scripts.


## Operational visibility

Metrics are always available privately: API_OPS_ADDR defaults to 127.0.0.1:9091, and WORKER_ADDR defaults to 127.0.0.1:8081. Both must use a literal loopback address; the public API never mounts /metrics. API readiness now verifies critical sender tables as well as connectivity, so apply migrations before launching processes. Receiver readiness remains independent.

make dev-observe-up copies reviewed configuration to the project-owned relayforge_observability_config Docker volume, then starts optional Prometheus/Grafana/Jaeger plus PostgreSQL/Redis. This avoids host configuration bind mounts. See [observability](observability.md) for local ports, temporary Grafana credentials, trace configuration, metrics, and platform limits. make test-observability-smoke runs the operator acceptance workflow with compiled processes and the live stack. dev-down stops all project profiles while preserving PostgreSQL/config volumes; local monitoring/traces are ephemeral.

TRACE_ENDPOINT is an explicit OTLP/HTTP traces URL; blank disables tracing but keeps metrics. TRACE_SAMPLE_RATIO defaults to 0.1 and is validated in [0,1]. Production trace URLs require HTTPS without embedded credentials/query/fragment. Sampling and a bounded asynchronous export queue prevent trace export from blocking durable delivery; collector errors appear separately. [Runbook](runbook.md) covers finding events, reading failures/backlog, replay, and dependency/exporter outages.


## Optional local browser console

After tenant provisioning and API startup, run `make run-console` in another terminal and open http://127.0.0.1:8091. Connect using a tenant API key. The development-only BFF keeps keys in short-lived server sessions. See [product interface](product-interface.md) for full workflow, custom ports, receiver setup and limits. `make build` now builds six binaries; `make test-console-smoke` exercises the real console/API/worker in disposable schemas.


## Benchmarks and hardening

`make benchmark BENCHMARK_OUTPUT=benchmarks/my-new-run` builds normal binaries and runs the local experiment matrix in disposable schemas. Start PostgreSQL first; output must be a new directory. `make benchmark-plans` seeds a larger disposable planner fixture, `make benchmark-profile` captures isolated Go CPU/allocation profiles, and `make security-check` runs the pinned official vulnerability scanner (network required). Read [methodology/results](../benchmarks/README.md) before interpreting numbers. Migration 9 adds an endpoint due-work index; index creation on an existing large database requires a planned migration window.

## Go environment troubleshooting

Install the Go version in `go.mod` and verify `go version`. If an old shell points at another toolchain or cache, reset those overrides in that shell:

```sh
unset GOROOT GOPATH GOCACHE
export GOPROXY="https://proxy.golang.org,direct"
go version
make build
```

The `make local-*` targets run previously built executables from ignored `bin/`. A fresh clone needs `make build` first. Source changes require rebuilding and restarting the affected process.
