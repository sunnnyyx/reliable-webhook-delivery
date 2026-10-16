-- +goose Up
CREATE INDEX deliveries_queued_age_idx ON deliveries(cycle_started_at,id)
WHERE status IN ('pending','retry_scheduled');

-- +goose Down
DROP INDEX deliveries_queued_age_idx;
