# ADR 0006: Per-slot claims, post-commit sends, and vetted outbound dialing

Status: accepted. Date: 2026-10-15.

Implementation update: Milestones 5–6 now implement lease recovery/sweeps, process-kill tests, replay, transactional receiver deduplication, and rotation verification. The original decision below records the worker-stage scope.

Use one synchronous claim/send/complete loop per available worker slot, not a prefetched batch. Claim endpoint before delivery with SKIP LOCKED, insert the started attempt and fresh lease atomically, commit, then decrypt/sign/send. Completion locks delivery and checks token/state/expiry after obtaining its lock; attempt and delivery changes commit together. Disable/delete share endpoint-first ordering, and deletion finishes started attempts as cancelled. A reserved claim can consume budget without making HTTP if preparation or process execution fails.

Use a dedicated HTTP/1 transport with no inherited proxy, redirect following, credential forwarding, or connection reuse. Validate every DNS answer then dial a vetted IP, preserving hostname for TLS/Host. Disabled reuse makes per-attempt revalidation explicit; profiling may justify a more complex safe reuse design later. The conservative special-address snapshot follows IANA registries and excludes some globally reachable special assignments. Allow only one explicit development localhost URL pinned to loopback; production refuses this configuration.

Follow Standard Webhooks HMAC/header conventions and verify the published Go signature vector. The receiver validates exact raw bytes/timestamp before acknowledgment, with no simulated business deduplication. Basic bounded failure scheduling is necessary for safe failure paths now; recovery/sweeper/fault-injection experiments remain milestone 5, transactional receiver deduplication/replay/rotation scenarios milestone 6.

Binary smoke tests use disposable migrated schemas in this project's local Compose database to avoid accidentally dispatching unrelated development work. The two-process test uses a local barrier proxy and one slot each to require both processes to participate, forwards exact bytes to the real receiver binary, and checks persisted worker IDs/attempts and body hashes.

Primary references verified at implementation: https://www.iana.org/assignments/iana-ipv4-special-registry, https://www.iana.org/assignments/iana-ipv6-special-registry, https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md, https://github.com/standard-webhooks/standard-webhooks/blob/main/libraries/go/webhook_test.go. No new dependencies were needed.
