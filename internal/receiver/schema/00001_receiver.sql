-- +goose Up
-- Receiver-owned tables deliberately have no sender tenant/event foreign keys.
CREATE TABLE receiver_processed_events (
 source text NOT NULL CHECK (source ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
 event_id uuid NOT NULL,
 body_sha256 bytea NOT NULL CHECK (octet_length(body_sha256)=32),
 first_delivery_id uuid NOT NULL,
 processed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(source,event_id)
);
CREATE TABLE receiver_business_events (
 source text NOT NULL,
 event_id uuid NOT NULL,
 event_type text NOT NULL CHECK (event_type ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(source,event_id),
 FOREIGN KEY(source,event_id) REFERENCES receiver_processed_events(source,event_id)
);

-- +goose Down
DROP TABLE receiver_business_events;
DROP TABLE receiver_processed_events;
