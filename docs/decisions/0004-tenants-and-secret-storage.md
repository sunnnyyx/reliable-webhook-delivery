# ADR 0004: Tenant-bound access and encrypted signing secrets

Status: accepted. Date: 2026-10-15.

Implementation update: Milestones 4–6 now implement protected outbound transport, dual signing, and receiver overlap/retirement verification. The original decision below records the earlier implementation stage.

Use a private database-backed admin CLI for tenant provisioning and key management. API tokens contain 32 random bytes; store SHA-256 hashes and prefixes. This is appropriate for high-entropy generated tokens; it is not a password-hashing scheme for human passwords. Authentication joins active keys with active tenants. Every endpoint query includes the authenticated tenant ID; cross-tenant objects look absent. No public provisioning route or body-supplied tenant identity exists.

Use AES-256-GCM for 32-byte signing secrets with random 12-byte nonces, 16-byte tags, and identity-bound AAD. Supply the single version-1 key outside PostgreSQL using ENCRYPTION_KEY. Never log plaintext credentials. API key generation and endpoint/secret creation return secrets only at creation; metadata excludes them.

Serialize endpoint count+insert on the tenant row. Endpoint mutations/rotation lock the endpoint; future dispatch must follow endpoint-before-delivery ordering. Authentication is a per-request authorization point; revocation does not cancel requests already authorized. Secrets and endpoints commit atomically.

Implement endpoint rotation records now because the endpoint API is present; hold old+new keys for 24 hours and reject another rotation during overlap. Actual dual signing and receiver overlap verification remain milestone 6. Registration URL syntax checks are intentionally distinct from connection-time SSRF enforcement in milestone 4. No outbound transport is implemented yet.

Use validated version-1 tenant-bound keyset cursors, not offsets. Cursors need not be secret or signed: authorization comes from tenant filtering and the bearer key. Reject malformed bodies before they reach storage; protect duplicate-key semantics and bound parsing work.
