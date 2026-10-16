# Durable event ingestion

Milestone 3 accepts JSON events into PostgreSQL and creates delivery work. [Milestone 4 workers](workers.md) now perform signed outbound delivery.

## Publish and inspect

Follow [setup](setup.md), including migrations and API encryption-key configuration. Provision a tenant and endpoint using [accounts and endpoints](accounts-and-endpoints.md), then start make run-api. Set TENANT_KEY to your provisioned token in the shell.

```sh
curl -i http://127.0.0.1:8080/v1/events \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-42-created-v1' \
  --data '{"type":"order.created","data":{"order_id":"42","amount_minor":12500,"currency":"INR"}}'
```

202 returns event_id, delivery_count, and accepted_at (UTC). Location points to the event. Idempotency-Replayed is false for fresh acceptance and true for a matching stored response. X-Request-ID identifies the current HTTP request; it is a header rather than part of the cached acceptance body.

The request requires exactly type and data. Type uses the same ASCII event-type grammar as subscriptions and cannot be wildcard. Data may be any JSON value, including object, array, scalar, or null. Incoming bytes are limited to 256 KiB. UTF-8, unique object keys (including nested objects), one JSON document, depth bounds, and canonical field names are checked before persistence. Unknown fields and invalid input return 400; oversize returns 413. Tenant identity comes solely from authentication.

## Idempotency contract

Idempotency-Key is required exactly once and contains 1–128 printable ASCII characters as received by HTTP. It is scoped to the authenticated tenant.

- The same unexpired key and exact incoming bytes return the original 202 body byte-for-byte, without creating more events or deliveries.
- Whitespace or JSON key-order changes produce different hashes and return 409, even if the JSON means the same thing.
- Unexpired reuse does not extend the original seven-day TTL.
- At expiry (including the exact expiry instant), a serialized transaction can replace the key record. The same key may then create a new event, even with different input. Old event/delivery history remains.
- If an HTTP response is lost, a timeout is ambiguous, or storage returns 503, retry the same bytes with the same key. Choosing a new key can create another event. Do not claim an event was rejected merely because its response was lost.

There is no in-memory idempotency cache. PostgreSQL's unique (tenant_id,idempotency_key) constraint arbitrates ownership across API processes. A conflict waits for an uncommitted owner, then a new READ COMMITTED statement locks/reads the committed response. Expired-key deletion/replacement is serialized by that same row/key uniqueness. A retry loop handles a record disappearing between conflict detection and row locking (for future bounded retention jobs); request context bounds the operation.

## Acceptance transaction

1. Authenticate, validate the bounded input, and hash the exact request bytes with SHA-256.
2. Reserve the unique key in a short database transaction; return an original response or mismatch if it already exists and is unexpired.
3. Select enabled, non-deleted matching endpoints in one ordered SQL statement. Exact subscriptions and wildcard both match; each endpoint appears once.
4. Generate/store the immutable event envelope and hash, and insert a pending delivery per selected endpoint with its URL snapshot.
5. Save the 202 response and commit; only then write the HTTP response.

A reservation's event foreign key is deferred because the reservation precedes event insertion. A deferred constraint trigger checks that a committed reservation has a complete saved response. Temporary incomplete rows are invisible to other transactions, and cannot commit. Commit-time errors return 503 and roll back all staged work.

The outgoing envelope adds stable id/type/created_at fields. Validated data JSON is embedded verbatim. Outgoing bytes live in bytea and are hashed separately from incoming request bytes. Automatic retries/replay must use these stored bytes; the event detail JSON view is for inspection, not a source for reconstructing signed bytes. Database triggers reject changes to event identity/type/body/hash/creation time and delivery identity/destination snapshots. Expiry metadata remains adjustable for future retention operations.

Zero matching endpoints is successful: the event and response persist with delivery_count 0. Registering another endpoint later does not change that event or create historical deliveries.

## Endpoint concurrency

Matching uses one READ COMMITTED SQL statement with FOR SHARE, locking endpoints in UUID order until acceptance commits. Concurrent registration may fall before or after that statement's snapshot. Concurrent endpoint updates/deletion either happen before a row is selected or wait until acceptance commits; PostgreSQL rechecks a concurrently updated row when acquiring its row lock. This is not a global ordering guarantee for unrelated endpoint changes.

The lock order is endpoint before its delivery rows. Deletion locks/soft-deletes the endpoint and cancels pending/retry-scheduled/in-flight delivery rows in the same transaction, clearing leases. Therefore a deletion that waits for acceptance also sees and cancels that acceptance's new pending deliveries. Disabled endpoints remain in history and hold existing pending work; changing URL/subscriptions affects future acceptance only. Re-enabling/dispatch and attempt cleanup races need the worker milestones; no in-flight network attempts exist yet.

Cancellation does not alter the original acceptance response or outgoing bytes. delivered and dead_letter are terminal and preserved if encountered. New work is never routed to a deleted endpoint. Tests observe the actual PostgreSQL lock wait to verify acceptance/delete coordination, rather than hoping to hit the race with sleeps.

## Read APIs

```sh
curl -sS 'http://127.0.0.1:8080/v1/events?limit=25' -H "Authorization: Bearer $TENANT_KEY"
curl -sS 'http://127.0.0.1:8080/v1/events/<event-uuid>' -H "Authorization: Bearer $TENANT_KEY"
curl -sS 'http://127.0.0.1:8080/v1/deliveries?status=pending&event_id=<event-uuid>' -H "Authorization: Bearer $TENANT_KEY"
curl -sS 'http://127.0.0.1:8080/v1/deliveries/<delivery-uuid>' -H "Authorization: Bearer $TENANT_KEY"
```

Event lists return metadata and outgoing-body SHA-256, omitting payloads. Event detail includes metadata, payload, and counts grouped by delivery status. Delivery lists/details expose destination snapshot, status, counters, and timestamps; no signing keys or lease tokens are exposed. Delivery filters are status, endpoint_id, and event_id. A valid filter identifying another tenant's object returns an empty list, just like any other nonmatching ID. Other-tenant/nonexistent direct resource routes return 404.

Lists default to 25, maximum 100, ordered by created_at descending then UUID descending. Pass next_cursor unchanged with the same filters; empty means completion. Event/delivery cursors bind their version, tenant, collection, and filter fingerprint to prevent accidental reuse across views. Cursors are not authorization tokens: every query still includes tenant predicates. Pagination is not a frozen snapshot across concurrent changes. No time-range filters are exposed yet.

Events currently receive nominal 30-day expiry metadata; cleanup jobs are not implemented and must later preserve live work and documented idempotency TTLs. Deliveries begin cycle 1 with zero attempts and pending status. Claims, signed sends, basic bounded retry scheduling, and attempt history are now implemented in milestone 4. Automatic expired-lease recovery and aged-work sweeps are implemented in milestone 5; dead-letter replay/receiver deduplication are implemented in milestone 6; see [replay and receiver](replay-and-receiver.md).

## Run checks

```sh
make test-race
make lint
# Set INTEGRATION_DATABASE_URL to the disposable database from setup.md.
make test-integration
make test-ingestion-smoke
```

Integration tests run real PostgreSQL, including ten same-key requests across two API handlers, exact response identity, mismatches, expiry/replacement contention with an injected clock, zero fan-out, immutable byte/destination guards, tenant foreign keys, rollback on fan-out or deferred commit failure, equal-timestamp pagination/filter validation, and endpoint deletion blocked behind acceptance's actual row lock.

Lost-response testing uses a deterministic failing HTTP ResponseWriter after commit and then retries the key; this simulates response loss rather than a real network cut. The binary smoke test additionally restarts the API between submission and retry, checks durable stored response/history and cancellation, and checks that captured API logs omit payloads and credentials. It creates synthetic history in a disposable schema, disables its Smoke tenant and drops the schema afterwards. It sends no webhook requests.


## Live backlog admission

Milestone 7 caps pending/retry_scheduled/in_flight deliveries per tenant (MAX_TENANT_BACKLOG, default 10000). New ingestion locks tenant admission before endpoint snapshots and counts the entire proposed fan-out. If it cannot fit, the whole transaction rolls back and returns 429 backlog_limit with Retry-After: 1. Retry the same key when capacity becomes available. An existing same-key acceptance is returned even at capacity; no-target events use no delivery capacity. Tenant disable is checked after acquiring the lifecycle lock. See [rate limits and scheduling](rate-limits.md).
