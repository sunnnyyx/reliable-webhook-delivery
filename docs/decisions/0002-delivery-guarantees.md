# ADR 0002: Finite attempts, immutable bytes, fenced completion

Status: accepted design. Date: 2026-10-13.

Use at-least-once attempts within a finite retry policy, with no ordering or exactly-once guarantee. Defaults: 10 attempts including the first, 24-hour cycle age, 10-second request timeout, 30-second lease, 2-second retry base and 1-hour cap with full jitter. A crash before send still consumes the reserved attempt.

Persist outgoing bytes once as bytea. Retry/replay retains event and delivery IDs, bytes, and URL snapshot. Sign each attempt with a fresh timestamp and current active endpoint keys. Replay starts a new cycle only for dead-letter work and preserves history.

Use lease tokens to fence database completion; perform HTTP outside locks. Receiver deduplication must commit its marker and demo effect together. This acknowledges ambiguous network success instead of concealing it.

Tenant-aware foreign keys and authenticated tenant filters are mandatory. Secrets are hashed (API keys) or authenticated-encrypted (signing keys), never logged. Connection-time SSRF checks are required before public deployment.
