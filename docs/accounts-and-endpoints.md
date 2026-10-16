# Tenants, credentials, and endpoints

Milestone 2 implements provisioning, authentication, endpoint CRUD, keyset pagination, secret storage, and the secret-rotation API. Milestone 3 adds [durable event ingestion](ingestion.md); [signed workers](workers.md) are implemented in milestone 4. See [OpenAPI](../api/openapi.json) for the implemented HTTP contract.

## Start locally

Follow [setup](setup.md), including PostgreSQL startup and migrations. The API now requires ENCRYPTION_KEY: a base64-encoded random 32-byte key. Generate it once, save it in ignored .env, and keep a secure backup:

```sh
python3 -c 'import secrets,base64; print(base64.b64encode(secrets.token_bytes(32)).decode())'
```

Copy the output into ENCRYPTION_KEY in .env. Treat this value as a secret. Do not overwrite an existing key: losing it makes stored signing secrets unreadable. API keys use SHA-256 and do not depend on this encryption key. The API and worker require the same key; admin/migration commands do not. Both fail clearly at startup if their required key is missing. Only encryption-key version 1 exists; changing the master key requires a future migration/key-management procedure, not a casual environment change.

```sh
make dev-up
make migrate-up
make build
make admin ARGS='tenant-create --name Demo'
```

The final command returns tenant_id, key_id, and api_key. The token is printed once by design. Save it securely; database rows contain a hash and a short prefix, not the recoverable token. The admin CLI uses database access and has no public HTTP equivalent.

```sh
make admin ARGS='key-create --tenant <tenant-uuid>'
make admin ARGS='key-revoke --tenant <tenant-uuid> --key <key-uuid>'
make admin ARGS='tenant-disable --tenant <tenant-uuid>'
```

Replace placeholders with IDs, not secret tokens. Revocation/disable operations are safe to repeat for an existing key/tenant. Revoked keys and disabled tenants fail subsequent authentication with 401. A request authenticated before revocation/disable may finish; authentication is checked once per request. New endpoint creation additionally checks the tenant is active under its transaction lock. Tenant re-enabling and API-key listing are not implemented.

## Endpoint workflow

Start make run-api. Set TENANT_KEY in your shell to the token received from provisioning. Do not commit it or store it in browser localStorage.

```sh
curl -sS http://127.0.0.1:8080/v1/endpoints \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"url":"https://hooks.example.com/orders","subscribed_event_types":["order.created"]}'
```

201 returns an endpoint object, a signing_secret, and a Location header. The signing secret is whsec_ followed by base64 encoding of 32 random bytes. Creation and rotation are the only HTTP responses containing a plaintext signing secret. If a creation response is lost, the existing secret cannot be retrieved; rotate it to obtain a new one. Endpoint creation has no idempotency contract in this version, so repeating creation may create another endpoint.

```sh
curl -sS 'http://127.0.0.1:8080/v1/endpoints?limit=25' -H "Authorization: Bearer $TENANT_KEY"
curl -sS 'http://127.0.0.1:8080/v1/endpoints/<endpoint-uuid>' -H "Authorization: Bearer $TENANT_KEY"
curl -sS -X PATCH 'http://127.0.0.1:8080/v1/endpoints/<endpoint-uuid>' \
  -H "Authorization: Bearer $TENANT_KEY" -H 'Content-Type: application/json' \
  --data '{"enabled":false}'
curl -sS -X POST 'http://127.0.0.1:8080/v1/endpoints/<endpoint-uuid>/rotate-secret' \
  -H "Authorization: Bearer $TENANT_KEY" -H 'Content-Type: application/json' --data '{}'
curl -i -X DELETE 'http://127.0.0.1:8080/v1/endpoints/<endpoint-uuid>' -H "Authorization: Bearer $TENANT_KEY"
```

Creation enables the endpoint. PATCH can change url, enabled, and subscribed_event_types; it requires at least one field. DELETE returns 204 and soft-deletes/disables the endpoint. Later get/patch/delete/rotation requests return 404, and listing excludes it. Milestone 3 now cancels unfinished delivery rows atomically with endpoint deletion. Disabling holds existing pending work; URL/subscription updates affect future events only. Milestone 4 workers check lifecycle under endpoint-first locks, fence completion, and retain cancelled attempt history after deletion.

Subscriptions accept 1–50 unique exact event types, or a sole wildcard ["*"]. Event types have 1–128 ASCII letters/digits/dot/underscore/hyphen and start with a letter/digit. More than 20 non-deleted endpoints per tenant returns 409; disabled endpoints still count. A tenant-row lock serializes count+insert so concurrent API processes cannot exceed the limit. Endpoint and initial encrypted secret commit together.

## Pagination and validation

GET /v1/endpoints returns data and next_cursor. Default limit is 25, maximum 100. Pass the returned cursor unchanged for the next page; an empty cursor means completion. Sorting is created_at descending, then UUID descending. Versioned cursors carry tenant/sort values and are validated; they are not authorization tokens. Every query separately filters the authenticated tenant. Pagination across concurrent changes is not a frozen snapshot.

Endpoint writes require Content-Type application/json and one UTF-8 JSON object. Bodies above 256 KiB return 413. Duplicate keys, trailing JSON/garbage, unknown or incorrectly cased field names, null values, empty patches, and excessive JSON nesting return 400. Tenant IDs in request bodies are rejected. Malformed pagination queries/cursors return 400. Unknown or other-tenant resource IDs return the same 404. Errors contain code/message/request_id, not SQL or credentials. Authorization failures return 401; unavailable storage returns sanitized 503. X-Request-ID and Cache-Control: no-store are set on responses.

## URL policy boundary

Registration currently requires HTTPS, port 443 (implicit or explicit), an ASCII DNS hostname, no IP literal, no credentials, and no fragment. Obvious local/reserved name suffixes, numeric destination spellings, invalid labels, and oversized URLs are rejected. Unicode hostnames must use their ASCII/Punycode representation. Milestone 4 adds one optional exact development localhost URL; production rejects it. See [workers](workers.md).

Registration validation remains syntax-only and does not resolve DNS. Milestone 4 adds connection-time DNS/IP vetting and direct approved-IP dialing in the worker, with redirects/proxies disabled and TLS/timeouts enforced. See [worker policy](workers.md). Public deployment remains deferred.

## Encryption and rotation

Signing secrets use AES-256-GCM with independent random 12-byte nonces. Authenticated associated data includes a version label plus tenant, endpoint, and secret IDs; moving ciphertext to another identity fails verification. PostgreSQL stores ciphertext, nonce, and key version. A composite foreign key independently prevents an endpoint secret from referencing another tenant's endpoint.

Rotation locks the endpoint, schedules the current key to retire in 24 hours, and creates a new encrypted key atomically. During overlap a second rotation returns 409, bounding active keys to old+new. Deleted endpoints cannot rotate. Old rows are retained. Milestone 4 signs with current active keys and provides a verifying receiver; end-to-end receiver migration/retired-key rotation scenarios pass in milestone 6. See [receiver setup and rotation](replay-and-receiver.md).

## Verification

```sh
make test-race
make lint
make test-integration
make test-smoke
make test-accounts-smoke
```

Set INTEGRATION_DATABASE_URL to the dedicated disposable database as described in setup. Integration tests create/drop isolated schemas and test cross-tenant routes/constraints, credential hashing/revocation, ciphertext identity binding, rollback on secret insert failure, pagination with equal timestamps, malformed requests, 24 concurrent endpoint creates, and eight concurrent rotation requests.

The accounts smoke test uses actual admin/API binaries with DATABASE_URL. It creates synthetic Smoke tenants, verifies CLI credentials, endpoint isolation, rotation, key revocation/tenant disable, and checks captured API logs for token/secret values. It disables synthetic tenants and drops its disposable schema afterwards. Use it only against development data. The accounts smoke sends no webhooks; the separate worker smoke contacts only the local compiled receiver through its test proxy.
