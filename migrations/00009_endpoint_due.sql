-- +goose Up
-- A tenant/endpoint claim must not walk another endpoint's earlier due work.
CREATE INDEX deliveries_endpoint_due_idx
 ON deliveries(tenant_id,endpoint_id,next_attempt_at,id)
 WHERE status IN ('pending','retry_scheduled');

-- +goose Down
DROP INDEX deliveries_endpoint_due_idx;
