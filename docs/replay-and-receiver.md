# Replay and receiver correctness

## Replay API

A new replay requires a dead_letter delivery, enabled/nondeleted endpoint, and an existing event whose expires_at is still in the future (nominally 30 days after acceptance). Pending, retry_scheduled, in_flight, delivered, and cancelled deliveries return 409 for a new replay key. Other tenants receive 404.

```sh
curl -sS -X POST "http://127.0.0.1:8080/v1/deliveries/$DELIVERY_ID/replay" \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: repair-attempt-1' \
  --data '{}'
```

202 returns id (replay UUID), delivery_id, new_cycle_number, and created_at. Location points to replay history. Idempotency-Replayed reports whether this is the saved acceptance. The JSON must be an empty object; unknown/null fields, duplicate headers/keys, invalid key length/characters, and malformed JSON are rejected. Keys contain 1–128 printable ASCII characters. Whitespace in the empty object has no semantic effect because there are no replay parameters.

The key is scoped to tenant + delivery. Concurrent requests with the same key return one identical acceptance. Distinct keys racing for one dead letter permit only one new cycle. Retry an ambiguous response with the same key and empty object. The saved acceptance remains valid when delivery progresses, endpoint configuration changes/deletes, or the event later expires; it does not enqueue again. Replay records currently have no expiration/cleanup. A new key always rechecks state/availability/retention. Replay history lifetime must remain tied to retained delivery history when cleanup is introduced.

Replay locks tenant admission before endpoint and delivery, matching the shared lifecycle order used by claims/ingestion; recovery/deletion need only endpoint then delivery. New replay requests need one live backlog slot (429 backlog_limit if unavailable); existing same-key receipts return before this capacity check. It increments cycle_number, resets cycle_attempt_count and the 24-hour cycle start, and queues immediately. Lifetime attempts and prior attempt rows remain. Event ID, delivery ID, outgoing raw bytes, original creation time, and url_snapshot remain unchanged. Current active endpoint keys are loaded when a worker claims. No attempt is created by the replay API itself. Replay-history insertion, capacity check, and cycle reset commit together; either failure rolls back both.

If the old URL is broken, changing the endpoint URL only affects future events. Submit a new event to use the new URL; replay never silently reroutes an old snapshot. An existing key is an acceptance receipt, not a current delivery-status response. Read GET /v1/deliveries/{id} for current status.

GET /v1/deliveries/{id}/replays lists audit records by created_at/id descending, with limit (default 25, max 100) and a cursor bound to tenant and delivery. Idempotency keys are omitted. GET /v1/deliveries/{id}/attempts retains all cycles, ascending by cycle/attempt. Replays of expired events are rejected even though cleanup has not yet removed the row.

## Receiver setup

The receiver requires its own explicit RECEIVER_DATABASE_URL. It never defaults to DATABASE_URL. For local development, create a dedicated database in this project's running PostgreSQL:

```sh
docker compose --env-file .env -f deploy/compose.yaml exec postgres \
  createdb -U relayforge relayforge_receiver_dev
```

In ignored .env, set RECEIVER_DATABASE_URL to your existing local connection credentials/port with database relayforge_receiver_dev, and set RECEIVER_SOURCE to a stable namespace such as demo-tenant-source. Save the endpoint's one-time secret in RECEIVER_SECRETS. Preserve ENCRYPTION_KEY and the source namespace across restarts. Then:

```sh
make migrate-up
make migrate-receiver-up
make run-receiver
```

Receiver migrations use receiver_goose_version and receiver-owned tables; sender migration versioning is independent. Neither auto-migration nor a database-creation operation runs during receiver startup. Tables have no sender database foreign keys. A deployment can use a separate PostgreSQL service, database, or restricted schema/role; local smoke tests use separate disposable schemas on one local service. This is logical separation, not a claim that separate production credentials/ACLs have been provisioned. Use a receiver runtime role limited to its tables in an actual deployment. migrate-receiver-down is destructive and restricted to explicit PROFILE=development.

RECEIVER_SOURCE is deployment configuration, not a caller-provided tenant ID. It binds one source/tenant and the configured accepted signing keys to a namespace. Run separate receiver configurations for unrelated tenants; accepting several tenants' keys under one namespace is not tenant routing. Never change the namespace to work around a duplicate, because that starts a new deduplication scope.

The receiver binds 127.0.0.1:8090 by default. GET /health/live is process health; GET /health/ready checks receiver storage/schema with a one-second bound. Webhook writes have a five-second context. Invalid storage configuration/schema prevents startup; persistence errors return 503 without SQL details.

## Transactional duplicate suppression

Verification reads bounded exact raw bytes, enforces canonical envelope fields, validates the five-minute timestamp window and HMAC, then enters receiver storage. No JSON reserialization occurs for signatures. Authentication is checked again on duplicates. The identity is (configured source, envelope event ID), so different delivery IDs for the same event also deduplicate.

One PostgreSQL transaction inserts receiver_processed_events (event/body hash/first delivery ID) and a receiver_business_events demo record. A unique event/source conflict waits for the other transaction's outcome. A valid already-processed event with identical bytes returns 204 and performs no new business write. Different bytes under the same identity return 409. If the business insert fails, the marker rolls back; retry may perform the effect. No raw payload or signing secrets are logged or stored in these demo tables. The sender continues to retain immutable outgoing bytes.

The demo effect is one local database record per source/event. This guarantees atomic local marker/effect behavior for this transaction; it does not make an external payment, email, or other remote action exactly once. Such effects need downstream idempotency keys or a transactional outbox with an idempotent consumer.

No receiver retention cleanup exists yet. Keep markers at least through every possible automatic retry and allowed replay window, including work still live beyond nominal retention. Deleting markers while a sender can resend permits effects to repeat. An explicit cleanup policy and retention jobs remain later lifecycle work.

## Key rotation

1. Configure the receiver with the old key.
2. POST /v1/endpoints/{id}/rotate-secret with {}. Save the new secret securely; it is displayed once. Further rotation conflicts during the 24-hour overlap.
3. Workers claiming during overlap sign with both active keys. The receiver can accept old-only, both comma-separated keys, or new-only during migration.
4. Restart the receiver with only the new key before overlap ends. Preserve its source/database.
5. After 24 hours, newly claimed sends contain only the new key's signature. Old-only receivers reject those sends; a new-only receiver rejects old-signed requests. Repair receiver key configuration and replay dead-lettered work with a new replay key if needed.

Retirement controls what the sender signs. A receiver still configured to trust an old key will continue trusting it; remove retired keys from receiver configuration. Work already authorized by a claim has an in-memory key snapshot; a later rotation does not revoke an HTTP request already authorized. Network policy still revalidates the destination on every send/retry/replay.

## Executed checks

make test-replay-smoke runs compiled API/worker/receiver: a local 400 dead-letters, twenty same-key replay requests create one new cycle, the new cycle delivers, old history/bytes remain, and duplicate requests leave one receiver business record. make test-worker-smoke runs two workers, restarts the compiled receiver, and verifies twelve redeliveries leave twelve business records.

PostgreSQL integration tests cover simultaneous same/distinct replay keys, lost API responses after a new API instance, tenant isolation/FKs, invalid state/retention/endpoint guards, rollback, replay cursor pages, concurrent receiver duplicates/source namespaces/changed bodies, business-write rollback, receiver migrations, and rotation. A real worker subprocess killed after a signed HTTP 204 recovers into a duplicate request with one durable business record. Test-only gates and a 300 ms lease accelerate this experiment; production has no crash failpoint switches. Rotation retirement is advanced only in isolated test SQL instead of waiting 24 hours. Published Standard Webhooks signature-vector tests remain in the unit suite. None of these tests is a benchmark or infrastructure failover exercise.
