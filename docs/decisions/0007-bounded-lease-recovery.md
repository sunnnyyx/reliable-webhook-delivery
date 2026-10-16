# 0007 — Bounded PostgreSQL lease recovery

Status: accepted, milestone 5.

Implementation update: Milestone 6 now adds transactional receiver deduplication and replay; its signed crash test proves duplicate requests leave one receiver business record. The original decision below records recovery-stage scope.

Every worker runs maintenance independently of its dispatch slots. Maintenance starts immediately, then waits five seconds after each pass. A pass has a five-second context and two separate budgets of 100 transactions: expired in_flight leases, then expired/exhausted pending or retry_scheduled work. Separate budgets allow queued ages to progress even with a stream of crashed claims. No leader election or additional service is needed.

Use endpoint -> delivery -> attempt locks with SKIP LOCKED, matching claim/deletion order. Recovery reads database time, finalizes exactly the attempt carrying the abandoned token, and clears/reschedules the delivery atomically. Recovery errors roll back that transition; already committed earlier transitions remain durable. Reserved attempts are never refunded. Disabled endpoints still age out. Completion fencing rejects the old token after lease expiry or transition.

Recovery reuses the existing retry planner: full jitter, ten reserved attempts, 24-hour cycles, terminal dead letters. It preserves IDs, payload bytes, URL snapshots, lifetime/cycle counters, and completed attempt history. A partial queued-age index complements the lease-expiry index. Scheduling is bounded work per pass, without a guaranteed recovery latency under contention/outage/backlog.

Crash gates exist only in test dispatch decorators. Subprocess tests kill workers after claim commit and after real HTTP success before completion, observing real accelerated lease expiry and retry eligibility. The latter demonstrates duplicate receiver effects; fencing cannot prevent an external request already sent. Transactional receiver deduplication and replay remain milestone 6. See workers.md and progress.md for experiments and scope.
