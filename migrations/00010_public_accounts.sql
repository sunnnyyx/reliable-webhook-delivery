-- +goose Up
ALTER TABLE api_keys ADD COLUMN expires_at timestamptz;
CREATE TABLE web_accounts (
    auth_user_id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL UNIQUE REFERENCES tenants(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE web_accounts;
ALTER TABLE api_keys DROP COLUMN expires_at;
