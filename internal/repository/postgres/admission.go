package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/endpoints"
)

var ErrBacklog = errors.New("tenant live delivery backlog limit reached")

// Existing tenants provide one admission mutex for ingestion/replay. This lock
// precedes endpoint/delivery locks. Terminal transitions can only free capacity.
func lockAdmission(ctx context.Context, tx pgx.Tx, tenant string) error {
	var id string
	e := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR NO KEY UPDATE`, tenant).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return endpoints.ErrNotFound
	}
	return e
}
func (s *Store) checkBacklog(ctx context.Context, tx pgx.Tx, tenant string, additional int) error {
	if additional == 0 {
		return nil
	}
	limit := s.MaxTenantBacklog
	if limit <= 0 {
		limit = 10000
	}
	if additional > limit {
		return ErrBacklog
	}
	var n int
	if e := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE tenant_id=$1 AND status IN ('pending','retry_scheduled','in_flight') LIMIT $2) live`, tenant, limit-additional+1).Scan(&n); e != nil {
		return e
	}
	if n+additional > limit {
		return ErrBacklog
	}
	return nil
}
