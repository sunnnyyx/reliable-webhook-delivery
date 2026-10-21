# Browser console

The default public demo now uses an isolated, no-login, in-memory playground. Run `make run-demo`; see [deployment instructions](deploy-free.md). The PostgreSQL backend and optional accounts remain separate modes.

The embedded red-and-white dashboard has two modes. Development preserves loopback-only API-key login. Production uses Supabase GitHub OAuth sign-in, automatically isolated workspaces, Secure cookies, and API-key management. Follow [public deployment](deploy-free.md) for identity/OAuth configuration and free-hosting limits.

Milestone 9 adds an embedded, buildless HTML/CSS/JavaScript console served by a separate Go binary. It uses the existing API for every operation. No additional JavaScript runtime, database tables, or migrations are needed. The development mode refuses non-loopback listeners/API origins. Hosted mode requires an explicit HTTPS public origin and Supabase identity configuration; upstream API traffic remains private.

## Open the console

From the project root, with prerequisites and `.env` from [setup](setup.md):

```sh
make dev-up
make migrate-up
make admin ARGS='tenant-create --name Demo'
```

Save the returned API key privately. The admin command shows it once. Provisioning and key management remain CLI operations; no database editing is required. For an existing tenant, create a replacement key using [accounts](accounts-and-endpoints.md).

In separate terminals in this project directory:

```sh
make run-api
make run-console
make run-worker
```

Open [the local console](http://127.0.0.1:8091) and paste your tenant API key into the password field. Use the literal configured address; `localhost` is deliberately not an alias, because the Host/Origin boundary must be exact. Defaults are `CONSOLE_ADDR=127.0.0.1:8091` and `CONSOLE_API_URL=http://127.0.0.1:8080`. If API_ADDR uses a different port, set CONSOLE_API_URL accordingly. All browser requests go to the console origin, and the console calls the API. CORS is unnecessary.

Use the Go release pinned in `go.mod`. Run `make build` to generate the six local executables; binaries are not distributed in the repository.

## Demo workflow

1. In **Endpoints**, add a receiver URL and comma-separated subscriptions (`order.created` or `*`). Save the one-time signing secret shown in the dialog and configure the receiver. Closing the dialog erases the displayed value. Pause/enable, rotate, and delete controls call the same authenticated API contracts. Rotation/deletion ask for confirmation.
2. For the local signed demo receiver, follow [worker/receiver setup](workers.md) and [receiver storage](replay-and-receiver.md). API and worker must both have the exact `DEV_RECEIVER_URL=http://localhost:<port>/webhooks` configured before startup. The receiver still needs its own database URL, migrations, trusted source, and signing secret. For a public receiver, normal HTTPS and outbound policy checks apply.
3. In **Events**, enter an event type and valid JSON data. An idempotency key is generated and stays visible. Publish, then inspect the returned event ID and target count. Repeating unchanged input with the same key returns the original acceptance. After a timeout, retain that key and unchanged data. Use **New key** only for a new logical event.
4. In **Overview**, refresh and filter by status, endpoint ID, or event ID. Lists show 25 rows per page with **Next page** when the API provides a cursor. Filtering/refresh starts from the newest page; no count is presented as a global total.
5. **View** opens a readable destination/status/attempt summary and paginated attempt/replay history. Raw identifiers and metadata are available in the Technical details disclosure. Events have a detail view including payload, summary, and any original trace ID, plus a delivery shortcut.
6. A **dead letter** exposes replay with a visible idempotency key and confirmation. Fix/enable the receiver first. The API still requires an unexpired event and available backlog capacity. Retry an uncertain replay with the same displayed key. An accepted replay keeps the original IDs, bytes, and destination snapshot. Close/refresh to inspect the new cycle. Closing/reopening generates a fresh replay key; save the displayed key if a response is uncertain. A new key is a new logical replay request.
7. **Disconnect** invalidates this browser's server session and clears rendered records/secrets. Absolute expiry or API revocation requires reconnecting. Draft event input remains only in the current page to allow an expired session to reconnect and retry with the same key; it disappears on page reload. Use Disconnect and close the tab on a shared computer.

Errors appear in an accessible status region; invalid JSON is rejected before submission. Mutating buttons show a working state and disable during the request. Network errors explain how to retain idempotency. Empty, unavailable, and loading lists have distinct messages. Overview supports pausable live refresh every eight seconds. Polling pauses when the tab is hidden, a detail dialog is open, a request is pending or another screen is active. Background refresh retains the current cursor/filter and previous visible rows; failed refresh is marked unavailable. Manual refresh returns to the newest page. This is polling, not instantaneous push delivery. Screens use native labels, keyboard-accessible controls/dialogs and responsive CSS; formal accessibility certification is not claimed.

## Session and request boundary

The key is transmitted once to `/session` in a JSON POST, validated through the existing API, and retained only in a bounded server map. It is never returned to the page, placed in a URL, written to disk/logs, or stored in localStorage/sessionStorage/IndexedDB. The login input clears immediately on submission. At most 128 sessions exist; expired entries are removed during login and access. Login attempts are limited globally to 10 per minute per console process. This is a local abuse bound, not a distributed authentication service.

Each session has a cryptographically random 256-bit opaque cookie (only its SHA-256 hash indexes server memory), a separate CSRF token, and an absolute 30-minute expiry. Cookie attributes are HttpOnly, SameSite=Strict, path=/, no Domain, max-age=1800. It intentionally has no Secure attribute because this development-only listener uses loopback HTTP. Production support requires a separate TLS/identity/session-store design; the binary rejects PROFILE=production. Loopback is not protection against malicious software/users already on the machine, and other services on the same host share the browser's cookie hostname boundary.

All state changes, including login, require exact Origin and Host checks; cookie-authenticated writes also require the session's CSRF header. Cross-site Fetch Metadata is rejected. Forwarded headers do not alter the trusted origin. Repeated login rotates this browser's existing session. Logout, expiration, upstream 401 and console restart invalidate sessions. Every proxied operation is authenticated anew by the existing API, so revocation/tenant disabling remain authoritative. Previously authorized requests can still finish while logout/revocation occurs; logout is not cancellation of committed API work. Multiple console processes have independent session maps and are not a supported HA setup.

The BFF permits only the endpoint/event/delivery routes used by the interface. API origins are fixed server configuration, literal loopback, with explicit port; no environment proxy, redirect following, arbitrary upstream URL, admin/metrics proxy or incoming authorization/cookie/baggage forwarding. Requests are bounded to 256 KiB, responses to 2 MiB, upstream calls to 6 seconds with bounded transport connection/header limits. Only content type, CSRF and idempotency inputs are used; downstream Authorization comes exclusively from the server session. Upstream cookies/Location headers are not exposed. Errors from transport/invalid upstream responses are sanitized. Selected request/trace/idempotency/retry response headers are retained for diagnosis.

HTML, JS, CSS and API/session responses use no-store, no-referrer, nosniff and frame-denial headers. CSP permits same-origin external assets and fetches, and prohibits inline script, framing, external assets and base URL changes. Dynamic API values are rendered with textContent, never inserted as HTML. Signing secrets are intentionally shown once for receiver setup and are cleared on dialog close/disconnection. Event payloads are authenticated tenant data and appear in detail views.

## Verification

```sh
make test-race
make test-console-smoke
```

The smoke requires local Compose PostgreSQL and creates disposable sender/receiver schemas using the existing helper. It drives actual console/API/worker binaries through endpoint creation, event acceptance/idempotency, HTTP 400/dead letter, replay/204, unchanged bytes, attempts/replay history, filters, cursor pagination, pause/enable, rotation, deletion, cross-tenant 404, revoked-key 401, logout and console restart. It inspects process logs for credential exclusion and drops its schemas afterward. The test receiver is a controlled HTTP fixture; transactional signed receiver behavior remains covered by the earlier worker/replay smoke suites.

Browser checks used a separate synthetic upstream fixture, not a production dataset: login, list/detail/history, replay confirmation, malformed JSON, publishing, endpoint form/one-time secret/pause, empty filters and logout. Real PostgreSQL-backed behavior is established separately by the binary smoke. No hosted deployment or performance benchmark is implied.


## Updated visual interface

The console now uses charcoal navigation, a blue/slate palette, a geometric relay-arrow SVG logo/favicon, a responsive overview, and an activity chart. Overview counts apply only to the current filtered/paginated response (maximum 25 deliveries), not all-time or fleet totals. The chart groups creation times from that same page into twelve rolling one-hour buckets; rows older than twelve hours are excluded from the chart. No synthetic chart values, success rates or uptime numbers are displayed. Status cards and chart update only after a successful read; unavailable initial data shows unknown counts. A failed background refresh retains the last view and timestamp with an unavailable indicator.

Quick status tabs use the existing API filter. Detailed status/endpoint/event filters expand on request. Publishing and endpoint actions remain explicit mutations; live refresh never submits a mutation. New-row highlights and the decorative handoff animation respect reduced-motion preferences. Login, session/CSRF protection, secret clearing, exact-byte/idempotency/replay contracts and backend storage remain unchanged.

Optional developer UI checks use Node's built-in runner: `make test-console-ui` (no npm packages; Node is not required to run the app). Five behavior checks cover real page-scoped counts/time bins, cursor-preserving refresh, polling pause guards, logout clearing and unavailable-data semantics. Real browser checks cover overview/filters/details, endpoint navigation, invalid JSON feedback, live timestamp updates and 390-pixel responsive layout. Dashboard counts come from the authenticated API, not sample metrics.

Assets are embedded at build time. Rebuild with `make build`, then restart your console process to load changed assets. The default address is http://127.0.0.1:8091. The current visual design uses a red-and-white palette, bold sans-serif headings, thin dividers, and a directional relay logo. Decorative motion respects reduced-motion preferences.
