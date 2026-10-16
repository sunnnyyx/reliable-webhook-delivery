-- +goose Up
-- Correlation metadata is separate from immutable outgoing event bytes.
CREATE TABLE event_trace_context (
 tenant_id uuid NOT NULL,
 event_id uuid NOT NULL,
 trace_id text NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
 span_id text NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$'),
 trace_flags smallint NOT NULL CHECK (trace_flags BETWEEN 0 AND 255),
 PRIMARY KEY(tenant_id,event_id),
 FOREIGN KEY(tenant_id,event_id) REFERENCES events(tenant_id,id)
);

-- +goose Down
DROP TABLE event_trace_context;
