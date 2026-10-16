package postgres

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/pagination"
	"time"
)

const deliveryColumns = "id,event_id,endpoint_id,url_snapshot,status,cycle_number,cycle_attempt_count,lifetime_attempt_count,next_attempt_at,cycle_started_at,lease_expires_at,last_error_code,last_http_status,delivered_at,created_at,updated_at"

func scanDelivery(row pgx.Row) (delivery.Delivery, error) {
	var v delivery.Delivery
	e := row.Scan(&v.ID, &v.EventID, &v.EndpointID, &v.URLSnapshot, &v.Status, &v.CycleNumber, &v.CycleAttemptCount, &v.LifetimeAttemptCount, &v.NextAttemptAt, &v.CycleStartedAt, &v.LeaseExpiresAt, &v.LastErrorCode, &v.LastHTTPStatus, &v.DeliveredAt, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	v.NextAttemptAt = v.NextAttemptAt.UTC()
	v.CycleStartedAt = v.CycleStartedAt.UTC()
	v.CreatedAt = v.CreatedAt.UTC()
	v.UpdatedAt = v.UpdatedAt.UTC()
	if v.LeaseExpiresAt != nil {
		v.LeaseExpiresAt = ptrTimeUTC(*v.LeaseExpiresAt)
	}
	if v.DeliveredAt != nil {
		v.DeliveredAt = ptrTimeUTC(*v.DeliveredAt)
	}
	return v, e
}
func (s *Store) GetDelivery(ctx context.Context, tenant, id string) (delivery.Delivery, error) {
	return scanDelivery(s.DB.QueryRow(ctx, "SELECT "+deliveryColumns+" FROM deliveries WHERE tenant_id=$1 AND id=$2", tenant, id))
}
func (s *Store) ListDeliveries(ctx context.Context, tenant string, limit int, cursor *pagination.Cursor, f delivery.Filters) ([]delivery.Delivery, string, error) {
	args := []any{tenant, limit + 1}
	q := "SELECT " + deliveryColumns + " FROM deliveries WHERE tenant_id=$1"
	if cursor != nil {
		args = append(args, cursor.Time, cursor.ID)
		q += " AND (created_at,id)<($3,$4)"
	}
	for _, term := range []struct{ column, value string }{{"status", f.Status}, {"endpoint_id", f.EndpointID}, {"event_id", f.EventID}} {
		if term.value != "" {
			args = append(args, term.value)
			q += fmt.Sprintf(" AND %s=$%d", term.column, len(args))
		}
	}
	q += " ORDER BY created_at DESC,id DESC LIMIT $2"
	rows, e := s.DB.Query(ctx, q, args...)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []delivery.Delivery{}
	for rows.Next() {
		v, e := scanDelivery(rows)
		if e != nil {
			return nil, "", e
		}
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		v := items[len(items)-1]
		next = pagination.Encode(tenant, "deliveries", f.Fingerprint(), v.CreatedAt, v.ID)
	}
	return items, next, nil
}

func ptrTimeUTC(t time.Time) *time.Time { v := t.UTC(); return &v }
