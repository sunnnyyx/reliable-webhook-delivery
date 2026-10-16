# 0011 — Local console with bounded server-side sessions

Status: accepted for milestone 9.

The optional interface must demonstrate the existing backend without storing long-lived tenant credentials in browser storage. We add a separate development-only Go binary serving embedded buildless assets and a narrow BFF. This keeps the API and worker contracts unchanged, needs no frontend toolchain, and adds no dependency or migration.

A submitted API key is validated through the API and kept only in process memory behind a 256-bit HttpOnly/SameSite cookie for 30 minutes. A separate CSRF token, exact Host/Origin and Fetch Metadata checks, restrictive CSP and text-only DOM construction protect the browser boundary. Each operation still invokes normal API authentication. Sessions, login attempts, request/response sizes and upstream time are bounded. The fixed upstream is literal loopback, without proxies/redirects or incoming cookie/authorization propagation.

We accept restart logout and single-process sessions for this local portfolio demo. HTTP cookies without Secure are permitted only on the loopback development console; production profile is rejected. Hosted TLS, identity provisioning, distributed sessions, additional roles and HA remain separate deployment decisions. Existing admin CLI provisioning plus the console completes the demo without manual database operations. Idempotency/replay semantics and one-time signing secrets remain visible rather than hidden behind automatic retries.

See [product interface](../product-interface.md) for exact limits, operating steps and verification.
