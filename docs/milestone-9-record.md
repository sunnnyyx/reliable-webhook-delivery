# Project progress

## Current milestone

9 — Optional product interface: complete. Await continuation before milestone 10 (benchmarks/hardening).

There are 12 milestones numbered 0–11. Milestones 0–9 are complete; milestones 10 and 11 remain. Prior backend and observability evidence is preserved in the [milestone 8 record](milestone-8-record.md).

## Completed work

A separate development-only Go console binary serves embedded buildless HTML/CSS/JavaScript and a narrow browser BFF. The interface provides endpoint creation, pause/enable, deletion and one-time signing-secret creation/rotation; event publishing with visible idempotency keys and acceptance receipts; event detail and delivery shortcuts; delivery status/endpoint/event filters; cursor pagination; attempt details; replay history and dead-letter replay with confirmation. Lists distinguish loading, empty and unavailable states. Mutating controls disable while working, errors include safe API diagnostics, and JSON errors are caught before submission. Desktop and narrow-screen layouts use labeled native controls and dialogs.

API keys are submitted once, validated through the API and held only in bounded console process memory. The browser gets a random HttpOnly/SameSite=Strict opaque cookie for 30 minutes plus a separate CSRF token. Exact Host/Origin, Fetch Metadata, CSRF and route allowlist checks protect the boundary. Each proxied operation still invokes API authentication. Sessions expire absolutely and invalidate on logout, key revocation/upstream 401, repeated login rotation and console restart. Keys are not written to browser storage, URLs or logs; the login input clears immediately. Dynamic values use textContent; restrictive CSP, no-store, no-referrer, nosniff and frame denial apply to every response.

The console binds only to literal loopback and accepts only a configured literal-loopback HTTP API origin. It rejects PROFILE=production. Its upstream client has no proxy or redirect following and bounded connection/header/body/time limits; browser cookies/authorization/trace baggage are not forwarded. Limits are 128 sessions, 10 login attempts per minute per process, 256 KiB proxy requests, 2 MiB responses and a 6-second upstream timeout. The local HTTP cookie deliberately lacks Secure; production TLS/identity/session architecture is deferred. No dependency, migration, existing API contract or worker behavior changed.

See [product interface](product-interface.md), [architecture](architecture.md) and [ADR 0011](decisions/0011-local-console-sessions.md) for operation and exact boundaries. The original specification and ignored private `.env` are unchanged.

## Changed files

cmd/console/main.go; internal/console/server.go and server_test.go; internal/console/assets/{index.html,style.css,app.js}; scripts/smoke-console.py; Makefile (sixth binary and run/test console targets); .env.example (optional console addresses); README.md; docs/setup.md; docs/architecture.md; docs/product-interface.md; docs/decisions/0011-local-console-sessions.md; docs/milestone-8-record.md and this progress record.

## Executed evidence

On 2026-10-16:

- `make fmt lint test-race` passed. The existing unit race suites pass alongside six new console tests covering cookie/session/CSRF behavior, exact proxy bytes/idempotency, header isolation, host/origin/route boundaries, expiry/revocation/login rotation, bounded request/session/login resources, sanitized upstream failures and local configuration. All six binaries built successfully.
- `make test-console-smoke` passed twice, including the final source revision. It uses actual compiled console/API/worker processes and PostgreSQL in disposable schemas. The workflow creates an endpoint, publishes/idempotently repeats an event, inspects an HTTP 400 dead letter, replays it to HTTP 204, verifies unchanged bytes/IDs, inspects both attempts and the replay receipt, checks filters/cursor pagination, pause/enable, rotation/deletion, cross-tenant 404, revoked-key 401, logout and restart. It inspects process logs for credential exclusion. Its controlled HTTP receiver is distinct from the earlier signed transactional receiver tests.
- Browser checks used a separate explicitly synthetic API fixture. Verified login/ledger, detail/attempt history, replay confirmation/new cycle, malformed JSON feedback, publish receipt, endpoint creation/one-time secret dialog/pause, empty filtered results and disconnect. Desktop screenshot reviewed and saved as preview (historical preview); phone-width login/event layouts also reviewed. Browser warnings/errors list was empty at the recorded check. The browser fixture is not production data or proof of backend durability; the real binary smoke establishes the backend connection separately.
- A browser session error on the initial preview opening was resolved by creating a fresh tab through the documented browser API. A replay click reported a tool timeout, but the next actual DOM inspection verified the accepted cycle and updated ledger. Neither transient tooling error is represented as a failed application operation.
- Original specification remained byte-identical to the supplied Desktop README; private `.env` remained mode 0600 and its sensitive values were absent from the new console sources. Python smoke syntax passed. No Go dependencies changed.

## Limits and next work

This is a local development console, not a production hosted identity system. Loopback HTTP and machine trust are explicit boundaries; sessions are single-process and disappear on restart. CLI tenant/key provisioning remains required but no direct database editing is needed. Running workers and a configured receiver remain necessary to demonstrate delivery; the console does not silently start either. No service worker, persistent browser storage, automatic mutation retries or UI polling is introduced.

Draft event input/idempotency keys stay only in the current page so an expired session can reconnect and retry. Disconnect clears rendered records/secrets; close/reload the tab to discard drafts. A replay key remains visible for retry while its detail dialog stays open; closing/reopening generates a new key, so save it after an uncertain response. Previously authorized API operations can finish during logout/revocation. Native browser confirmation is used for destructive controls/replay. Formal accessibility certification and exhaustive cross-browser/mobile device testing are not claimed.

Earlier integration and seven pre-console binary smoke results remain recorded in milestone 8; they were not all rerun for this isolated console addition. Current verification is the complete unit race suite, formatting/vet, six builds, new real console smoke and the browser checks above. No throughput/SLO/large-history benchmark, production TLS/session store, HA, retention job, deployment, CI, remote Git operation or commit is claimed.

Next: milestone 10 — benchmarks and hardening. Milestone 11 covers deployment and portfolio delivery. Stop here until continuation.

Final cleanup removed this project’s PostgreSQL container/network while preserving its data volume. The temporary synthetic browser fixture and console were no longer listening on ports 18090/8091 after the interrupted preview session. All binary-smoke child processes stopped and its disposable schemas were dropped by the fixture cleanup. No test server is left running; use the interface guide to start the real console.
