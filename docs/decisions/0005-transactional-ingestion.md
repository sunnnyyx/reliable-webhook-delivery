# ADR 0005: Atomic ingestion, key reservation, and endpoint lifecycle locks

Status: accepted. Date: 2026-10-15.

Implementation update: Attempt views and idempotent replay are now implemented through milestone 6. The original decision below records ingestion-stage scope.

Use one READ COMMITTED PostgreSQL transaction for idempotency reservation, exact outgoing body storage, matching endpoint snapshots, delivery creation, and the stored 202 response. Reserve with INSERT ... ON CONFLICT DO NOTHING; lock/read conflicts in a new statement, returning original bytes or a 409. Expired keys are locked, deleted, and re-reserved in that transaction. Database uniqueness, not a process mutex, coordinates concurrent API replicas.

The provisional reservation has nullable response fields within the transaction. A deferred tenant-aware event foreign key and deferred constraint trigger prevent incomplete reservations from committing. This adds a small PL/pgSQL function instead of weakening the committed-row invariant. Tests force both statement-time and deferred commit-time failure and verify there is no acceptance or partial work.

Persist one immutable bytea envelope, with a separate body hash and immutable destination snapshots. Hash incoming bytes exactly, including whitespace/key order. Data supports all JSON values, including null, and is embedded verbatim after strict duplicate-key/size/depth validation. SQL triggers reject changes to event identity/body and delivery identity/URL.

Use a single ordered matching SELECT FOR SHARE. This permits concurrent publishers but conflicts with endpoint mutation/deletion. Delete locks the endpoint first and cancels unfinished deliveries in the same transaction. One snapshot governs matching; concurrent registration may fall on either side, and READ COMMITTED row-lock rechecks can observe a concurrent endpoint update. No network call occurs in acceptance.

Use injected clock/identifier functions for deterministic expiry and lock-race tests. Production defaults are UTC wall clock (microsecond precision matching PostgreSQL) and cryptographic UUIDs; no HTTP/environment failpoint switches are exposed. Request contexts bound database waits. Nominal TTLs are seven days for ingestion keys and 30 days for event history; retention jobs remain later work and must preserve live deliveries.

Expose tenant-scoped event/delivery views and collection/filter-bound keyset cursors. Attempt routes and replay wait for their own milestones. A saved acceptance is durable work, not proof of successful receiver processing.
