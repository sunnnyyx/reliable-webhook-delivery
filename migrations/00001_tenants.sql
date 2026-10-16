-- +goose Up
CREATE TABLE tenants (
 id uuid PRIMARY KEY,
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
 created_at timestamptz NOT NULL DEFAULT now(),
 disabled_at timestamptz
);

-- +goose Down
DROP TABLE tenants;
