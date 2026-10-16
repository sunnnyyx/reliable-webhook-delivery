-- +goose Up
CREATE TABLE delivery_attempts (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 delivery_id uuid NOT NULL,
 cycle_number integer NOT NULL CHECK(cycle_number>=1),
 attempt_number integer NOT NULL CHECK(attempt_number>=1),
 lease_token uuid NOT NULL UNIQUE,
 worker_id uuid NOT NULL,
 started_at timestamptz NOT NULL,
 finished_at timestamptz,
 outcome text NOT NULL CHECK(outcome IN ('started','success','retryable_failure','permanent_failure','lease_expired','rate_limited','cancelled')),
 http_status integer CHECK(http_status BETWEEN 100 AND 599),
 latency_ms bigint CHECK(latency_ms>=0),
 error_code text,
 UNIQUE(delivery_id,cycle_number,attempt_number),
 FOREIGN KEY(tenant_id,delivery_id) REFERENCES deliveries(tenant_id,id),
 CHECK((outcome='started' AND finished_at IS NULL) OR (outcome<>'started' AND finished_at IS NOT NULL))
);
-- Unique delivery/cycle/attempt index also serves tenant-authorized attempt history.

-- +goose Down
DROP TABLE delivery_attempts;
