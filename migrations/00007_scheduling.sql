-- +goose Up
CREATE TABLE tenant_dispatch_state (
 tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
 last_selected_at timestamptz NOT NULL DEFAULT '-infinity',
 dispatch_after timestamptz NOT NULL DEFAULT '-infinity'
);
INSERT INTO tenant_dispatch_state(tenant_id) SELECT id FROM tenants;
-- +goose StatementBegin
CREATE FUNCTION initialize_tenant_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO tenant_dispatch_state(tenant_id) VALUES(NEW.id);
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER initialize_tenant_dispatch AFTER INSERT ON tenants FOR EACH ROW EXECUTE FUNCTION initialize_tenant_dispatch();
CREATE INDEX tenant_dispatch_order_idx ON tenant_dispatch_state(last_selected_at,tenant_id);
CREATE INDEX deliveries_tenant_live_idx ON deliveries(tenant_id,id) WHERE status IN ('pending','retry_scheduled','in_flight');
CREATE INDEX deliveries_tenant_flight_idx ON deliveries(tenant_id) WHERE status='in_flight';

-- +goose Down
DROP INDEX deliveries_tenant_flight_idx;
DROP INDEX deliveries_tenant_live_idx;
DROP TRIGGER initialize_tenant_dispatch ON tenants;
DROP FUNCTION initialize_tenant_dispatch();
DROP TABLE tenant_dispatch_state;
