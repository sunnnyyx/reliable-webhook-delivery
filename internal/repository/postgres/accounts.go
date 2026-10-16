package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"relayforge/internal/endpoints"
	"relayforge/internal/netpolicy"
	"relayforge/internal/ratelimit"
	"relayforge/internal/security"
	"relayforge/internal/telemetry"
	"strings"
	"time"
	"unicode/utf8"
)

type Store struct {
	Telemetry *telemetry.Runtime

	Limiter           ratelimit.Limiter
	MaxTenantInFlight int
	MaxTenantBacklog  int

	DB     *pgxpool.Pool
	Vault  *security.Vault
	Policy *netpolicy.Policy
	Now    func() time.Time
	NewID  func() string
}
type Credential struct {
	TenantID string `json:"tenant_id"`
	KeyID    string `json:"key_id"`
	APIKey   string `json:"api_key"`
}

func validName(name string) bool {
	return len(name) > 0 && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 200 && strings.TrimSpace(name) == name
}
func (s *Store) Provision(ctx context.Context, name string) (Credential, error) {
	if !validName(name) {
		return Credential{}, errors.New("name must contain 1–200 characters without outer whitespace")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return Credential{}, e
	}
	defer tx.Rollback(ctx)
	tenant := security.ID()
	if _, e = tx.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,$2)", tenant, name); e != nil {
		return Credential{}, e
	}
	c, e := insertKey(ctx, tx, tenant)
	if e != nil {
		return c, e
	}
	e = tx.Commit(ctx)
	return c, e
}
func insertKey(ctx context.Context, tx pgx.Tx, tenant string) (Credential, error) {
	token, hash, e := security.Token()
	if e != nil {
		return Credential{}, e
	}
	c := Credential{tenant, security.ID(), token}
	_, e = tx.Exec(ctx, "INSERT INTO api_keys(id,tenant_id,key_prefix,key_hash) VALUES($1,$2,$3,$4)", c.KeyID, tenant, token[:11], hash)
	return c, e
}
func (s *Store) CreateKey(ctx context.Context, tenant string) (Credential, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return Credential{}, e
	}
	defer tx.Rollback(ctx)
	var id string
	e = tx.QueryRow(ctx, "SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR UPDATE", tenant).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return Credential{}, endpoints.ErrNotFound
	}
	if e != nil {
		return Credential{}, e
	}
	c, e := insertKey(ctx, tx, tenant)
	if e != nil {
		return c, e
	}
	e = tx.Commit(ctx)
	return c, e
}
func (s *Store) RevokeKey(ctx context.Context, tenant, key string) error {
	tag, e := s.DB.Exec(ctx, "UPDATE api_keys SET revoked_at=coalesce(revoked_at,now()) WHERE tenant_id=$1 AND id=$2", tenant, key)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return endpoints.ErrNotFound
	}
	return nil
}
func (s *Store) DisableTenant(ctx context.Context, tenant string) error {
	tag, e := s.DB.Exec(ctx, "UPDATE tenants SET disabled_at=coalesce(disabled_at,now()) WHERE id=$1", tenant)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return endpoints.ErrNotFound
	}
	return nil
}
func (s *Store) Authenticate(ctx context.Context, token string) (string, error) {
	if !security.ValidToken(token) {
		return "", endpoints.ErrNotFound
	}
	var tenant string
	e := s.DB.QueryRow(ctx, `SELECT k.tenant_id FROM api_keys k JOIN tenants t ON t.id=k.tenant_id WHERE k.key_hash=$1 AND k.revoked_at IS NULL AND t.disabled_at IS NULL`, security.Hash(token)).Scan(&tenant)
	if errors.Is(e, pgx.ErrNoRows) {
		e = endpoints.ErrNotFound
	}
	return tenant, e
}
