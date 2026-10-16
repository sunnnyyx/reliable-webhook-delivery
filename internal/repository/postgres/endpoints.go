package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/endpoints"
	"relayforge/internal/netpolicy"
	"relayforge/internal/security"
)

const endpointColumns = "id,url,enabled,subscribed_event_types,created_at,updated_at"

func scanEndpoint(row pgx.Row) (endpoints.Endpoint, error) {
	var v endpoints.Endpoint
	e := row.Scan(&v.ID, &v.URL, &v.Enabled, &v.Subscriptions, &v.CreatedAt, &v.UpdatedAt)
	v.CreatedAt = v.CreatedAt.UTC()
	v.UpdatedAt = v.UpdatedAt.UTC()
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	return v, e
}
func (s *Store) GetEndpoint(ctx context.Context, tenant, id string) (endpoints.Endpoint, error) {
	return scanEndpoint(s.DB.QueryRow(ctx, "SELECT "+endpointColumns+" FROM endpoints WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL", tenant, id))
}
func (s *Store) CreateEndpoint(ctx context.Context, tenant string, input endpoints.Create) (endpoints.Endpoint, string, error) {
	if e := s.validateEndpoint(input.URL, input.Subscriptions); e != nil {
		return endpoints.Endpoint{}, "", ValidationError{e}
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return endpoints.Endpoint{}, "", e
	}
	defer tx.Rollback(ctx)
	// Serialize count+insert against the tenant row, including across API replicas.
	var id string
	e = tx.QueryRow(ctx, "SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR UPDATE", tenant).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	if e != nil {
		return endpoints.Endpoint{}, "", e
	}
	var n int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM endpoints WHERE tenant_id=$1 AND deleted_at IS NULL", tenant).Scan(&n); e != nil {
		return endpoints.Endpoint{}, "", e
	}
	if n >= 20 {
		return endpoints.Endpoint{}, "", endpoints.ErrLimit
	}
	id = security.ID()
	v, e := scanEndpoint(tx.QueryRow(ctx, "INSERT INTO endpoints(id,tenant_id,url,subscribed_event_types) VALUES($1,$2,$3,$4) RETURNING "+endpointColumns, id, tenant, input.URL, input.Subscriptions))
	if e != nil {
		return v, "", e
	}
	secret, e := s.insertSecret(ctx, tx, tenant, id)
	if e != nil {
		return v, "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return v, "", e
	}
	return v, secret, nil
}
func (s *Store) insertSecret(ctx context.Context, tx pgx.Tx, tenant, endpoint string) (string, error) {
	plain := make([]byte, 32)
	if _, e := rand.Read(plain); e != nil {
		return "", e
	}
	id := security.ID()
	cipher, nonce, e := s.Vault.Encrypt(tenant, endpoint, id, plain)
	if e != nil {
		return "", e
	}
	_, e = tx.Exec(ctx, "INSERT INTO endpoint_secrets(id,tenant_id,endpoint_id,encrypted_secret,nonce,encryption_key_version) VALUES($1,$2,$3,$4,$5,1)", id, tenant, endpoint, cipher, nonce)
	if e != nil {
		return "", e
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(plain), nil
}
func (s *Store) UpdateEndpoint(ctx context.Context, tenant, id string, input endpoints.Patch) (endpoints.Endpoint, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return endpoints.Endpoint{}, e
	}
	defer tx.Rollback(ctx)
	v, e := scanEndpoint(tx.QueryRow(ctx, "SELECT "+endpointColumns+" FROM endpoints WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE", tenant, id))
	if e != nil {
		return v, e
	}
	if input.URL != nil {
		v.URL = *input.URL
	}
	if input.Enabled != nil {
		v.Enabled = *input.Enabled
	}
	if input.Subscriptions != nil {
		v.Subscriptions = *input.Subscriptions
	}
	if e = s.validateEndpoint(v.URL, v.Subscriptions); e != nil {
		return v, ValidationError{e}
	}
	v, e = scanEndpoint(tx.QueryRow(ctx, "UPDATE endpoints SET url=$3,enabled=$4,subscribed_event_types=$5,updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING "+endpointColumns, tenant, id, v.URL, v.Enabled, v.Subscriptions))
	if e != nil {
		return v, e
	}
	e = tx.Commit(ctx)
	return v, e
}

type ValidationError struct{ error }

func (s *Store) DeleteEndpoint(ctx context.Context, tenant, id string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, "UPDATE endpoints SET deleted_at=clock_timestamp(),enabled=false,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL", tenant, id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return endpoints.ErrNotFound
	}
	if _, e = tx.Exec(ctx, `UPDATE deliveries SET status='cancelled',lease_token=NULL,lease_expires_at=NULL,last_error_code='endpoint_deleted',updated_at=clock_timestamp() WHERE tenant_id=$1 AND endpoint_id=$2 AND status IN ('pending','retry_scheduled','in_flight')`, tenant, id); e != nil {
		return e
	}
	// Finish a reserved attempt as cancelled before releasing the deleted lease.
	if _, e = tx.Exec(ctx, `UPDATE delivery_attempts a SET outcome='cancelled',finished_at=clock_timestamp(),error_code='endpoint_deleted' FROM deliveries d WHERE a.tenant_id=$1 AND d.tenant_id=a.tenant_id AND d.id=a.delivery_id AND d.endpoint_id=$2 AND d.status='cancelled' AND a.outcome='started'`, tenant, id); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) ListEndpoints(ctx context.Context, tenant string, limit int, cursor *endpoints.Cursor) ([]endpoints.Endpoint, string, error) {
	args := []any{tenant, limit + 1}
	query := "SELECT " + endpointColumns + " FROM endpoints WHERE tenant_id=$1 AND deleted_at IS NULL"
	if cursor != nil {
		query += " AND (created_at,id)<($3,$4)"
		args = append(args, cursor.Time, cursor.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT $2"
	rows, e := s.DB.Query(ctx, query, args...)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []endpoints.Endpoint{}
	for rows.Next() {
		v, e := scanEndpoint(rows)
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
		next = endpoints.EncodeCursor(tenant, items[len(items)-1])
	}
	return items, next, nil
}
func (s *Store) RotateSecret(ctx context.Context, tenant, id string) (string, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	var locked string
	e = tx.QueryRow(ctx, "SELECT id FROM endpoints WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE", tenant, id).Scan(&locked)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	if e != nil {
		return "", e
	}
	var overlap bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM endpoint_secrets WHERE tenant_id=$1 AND endpoint_id=$2 AND retire_after>now())", tenant, id).Scan(&overlap); e != nil {
		return "", e
	}
	if overlap {
		return "", endpoints.ErrOverlap
	}
	if _, e = tx.Exec(ctx, "UPDATE endpoint_secrets SET retire_after=now()+interval '24 hours' WHERE tenant_id=$1 AND endpoint_id=$2 AND retire_after IS NULL", tenant, id); e != nil {
		return "", e
	}
	secret, e := s.insertSecret(ctx, tx, tenant, id)
	if e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return secret, nil
}

func (s *Store) validateEndpoint(url string, types []string) error {
	validate := netpolicy.ValidateURL
	if s.Policy != nil {
		validate = s.Policy.ValidateURL
	}
	if e := validate(url); e != nil {
		return e
	}
	return endpoints.ValidateSubscriptions(types)
}
