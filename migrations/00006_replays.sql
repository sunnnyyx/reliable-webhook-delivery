-- +goose Up
CREATE TABLE delivery_replays (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 delivery_id uuid NOT NULL,
 idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128 AND idempotency_key !~ '[^ -~]'),
 new_cycle_number integer NOT NULL CHECK (new_cycle_number>=2),
 created_at timestamptz NOT NULL,
 FOREIGN KEY(tenant_id,delivery_id) REFERENCES deliveries(tenant_id,id),
 UNIQUE(tenant_id,delivery_id,idempotency_key),
 UNIQUE(delivery_id,new_cycle_number)
);
CREATE INDEX delivery_replays_list_idx ON delivery_replays(tenant_id,delivery_id,created_at DESC,id DESC);

-- +goose Down
DROP TABLE delivery_replays;
