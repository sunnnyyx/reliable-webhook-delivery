-- +goose Up
CREATE TABLE api_keys (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 key_prefix text NOT NULL,
 key_hash bytea NOT NULL UNIQUE CHECK (octet_length(key_hash)=32),
 created_at timestamptz NOT NULL DEFAULT now(),
 revoked_at timestamptz
);
CREATE INDEX api_keys_tenant_idx ON api_keys(tenant_id, created_at DESC);
CREATE TABLE endpoints (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 url text NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
 enabled boolean NOT NULL DEFAULT true,
 subscribed_event_types text[] NOT NULL CHECK (cardinality(subscribed_event_types) BETWEEN 1 AND 50 AND array_position(subscribed_event_types,NULL) IS NULL),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 UNIQUE(tenant_id,id)
);
CREATE INDEX endpoints_tenant_list_idx ON endpoints(tenant_id,created_at DESC,id DESC) WHERE deleted_at IS NULL;
CREATE TABLE endpoint_secrets (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 endpoint_id uuid NOT NULL,
 encrypted_secret bytea NOT NULL CHECK (octet_length(encrypted_secret)=48),
 nonce bytea NOT NULL CHECK (octet_length(nonce)=12),
 encryption_key_version smallint NOT NULL CHECK (encryption_key_version=1),
 active_from timestamptz NOT NULL DEFAULT now(),
 retire_after timestamptz CHECK (retire_after IS NULL OR retire_after>active_from),
 FOREIGN KEY(tenant_id,endpoint_id) REFERENCES endpoints(tenant_id,id)
);
CREATE INDEX endpoint_secrets_active_idx ON endpoint_secrets(tenant_id,endpoint_id,active_from);

-- +goose Down
DROP TABLE endpoint_secrets;
DROP TABLE endpoints;
DROP TABLE api_keys;
