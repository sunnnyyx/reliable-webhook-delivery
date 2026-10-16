-- +goose Up
CREATE TABLE events (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 event_type text NOT NULL CHECK (event_type ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
 outgoing_body bytea NOT NULL CHECK (octet_length(outgoing_body) BETWEEN 1 AND 262656),
 body_sha256 bytea NOT NULL CHECK (octet_length(body_sha256)=32),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at>created_at),
 UNIQUE(tenant_id,id)
);
CREATE INDEX events_tenant_list_idx ON events(tenant_id,created_at DESC,id DESC);
CREATE INDEX events_expiry_idx ON events(expires_at,id);
CREATE TABLE ingestion_idempotency (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128 AND idempotency_key !~ '[^ -~]'),
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 event_id uuid NOT NULL,
 response_status smallint CHECK (response_status=202),
 response_body bytea CHECK (octet_length(response_body) BETWEEN 2 AND 2048),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at>created_at),
 PRIMARY KEY(tenant_id,idempotency_key),
 -- Reservation precedes the event insert, but a committed reservation must reference it.
 FOREIGN KEY(tenant_id,event_id) REFERENCES events(tenant_id,id) DEFERRABLE INITIALLY DEFERRED,
 CHECK ((response_status IS NULL)=(response_body IS NULL))
);
CREATE INDEX ingestion_expiry_idx ON ingestion_idempotency(expires_at);
-- A response is temporarily absent while a transaction owns its reservation.
-- Defer validation until commit and read the final row, not the provisional NEW tuple.
-- +goose StatementBegin
CREATE FUNCTION require_ingestion_response() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS (SELECT 1 FROM ingestion_idempotency
            WHERE tenant_id=NEW.tenant_id AND idempotency_key=NEW.idempotency_key
              AND (response_status IS NULL OR response_body IS NULL)) THEN
   RAISE EXCEPTION 'incomplete ingestion reservation' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER ingestion_response_complete
 AFTER INSERT OR UPDATE ON ingestion_idempotency DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION require_ingestion_response();
CREATE TABLE deliveries (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 event_id uuid NOT NULL,
 endpoint_id uuid NOT NULL,
 url_snapshot text NOT NULL CHECK (length(url_snapshot) BETWEEN 1 AND 2048),
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','in_flight','retry_scheduled','delivered','dead_letter','cancelled')),
 cycle_number integer NOT NULL DEFAULT 1 CHECK (cycle_number>=1),
 cycle_attempt_count integer NOT NULL DEFAULT 0 CHECK (cycle_attempt_count>=0),
 lifetime_attempt_count integer NOT NULL DEFAULT 0 CHECK (lifetime_attempt_count>=cycle_attempt_count),
 next_attempt_at timestamptz NOT NULL,
 cycle_started_at timestamptz NOT NULL,
 lease_token uuid,
 lease_expires_at timestamptz,
 last_error_code text,
 last_http_status integer CHECK (last_http_status BETWEEN 100 AND 599),
 delivered_at timestamptz,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 UNIQUE(event_id,endpoint_id),
 UNIQUE(tenant_id,id),
 FOREIGN KEY(tenant_id,event_id) REFERENCES events(tenant_id,id),
 FOREIGN KEY(tenant_id,endpoint_id) REFERENCES endpoints(tenant_id,id),
 CHECK ((status='in_flight' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status<>'in_flight' AND lease_token IS NULL AND lease_expires_at IS NULL))
);
CREATE INDEX deliveries_due_idx ON deliveries(next_attempt_at,id) WHERE status IN ('pending','retry_scheduled');
CREATE INDEX deliveries_lease_expiry_idx ON deliveries(lease_expires_at,id) WHERE status='in_flight';
CREATE INDEX deliveries_tenant_list_idx ON deliveries(tenant_id,created_at DESC,id DESC);
CREATE INDEX deliveries_endpoint_list_idx ON deliveries(tenant_id,endpoint_id,created_at DESC,id DESC);
-- +goose StatementBegin
CREATE FUNCTION protect_event_bytes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.tenant_id,NEW.event_type,NEW.outgoing_body,NEW.body_sha256,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.event_type,OLD.outgoing_body,OLD.body_sha256,OLD.created_at) THEN
   RAISE EXCEPTION 'event identity and body are immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER immutable_event BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION protect_event_bytes();
-- +goose StatementBegin
CREATE FUNCTION protect_delivery_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.tenant_id,NEW.event_id,NEW.endpoint_id,NEW.url_snapshot,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.tenant_id,OLD.event_id,OLD.endpoint_id,OLD.url_snapshot,OLD.created_at) THEN
   RAISE EXCEPTION 'delivery identity and destination are immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER immutable_delivery BEFORE UPDATE ON deliveries FOR EACH ROW EXECUTE FUNCTION protect_delivery_snapshot();

-- +goose Down
DROP TABLE deliveries;
DROP FUNCTION protect_delivery_snapshot();
DROP TABLE ingestion_idempotency;
DROP FUNCTION require_ingestion_response();
DROP TABLE events;
DROP FUNCTION protect_event_bytes();
