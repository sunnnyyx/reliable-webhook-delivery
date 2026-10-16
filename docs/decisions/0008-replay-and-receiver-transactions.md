# 0008 — Replay receipts and receiver-owned deduplication

Status: accepted, milestone 6.

Replay is a new finite delivery cycle on the existing immutable delivery. Lock endpoint -> delivery, look for an existing tenant/delivery/key acceptance, then validate dead-letter state, endpoint availability, and retained event. Reset cycle state and insert the audit record together. A saved key returns its original immutable acceptance even when mutable state later changes; a distinct key must pass the guards. Empty-object requests avoid implicit rerouting or mutable replay options. Audit history and all prior attempts remain visible with tenant-bound pagination.

Receiver state is independent of sender rows and migration history. An explicit database URL and trusted source namespace bind a receiver deployment to its accepted keys. Unique source/event insertion and the demo business record commit together. Conflict means same raw-body hash or a 409; authenticated same-byte duplicates acknowledge with 204. Verify signatures before any duplicate shortcut. Namespaces come from configuration and must not group unrelated tenant keys without deliberate routing.

Keep marker retention at least as long as possible sender resends/replays. No cleanup job exists yet. Local demo database writes are atomic; external effects require downstream idempotency or an outbox. Process-kill, concurrency, rollback, compiled restart/replay, and current-key/retirement tests verify these boundaries without production fault switches or exactly-once external-action claims.
