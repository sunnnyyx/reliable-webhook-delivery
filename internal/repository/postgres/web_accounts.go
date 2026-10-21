package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/security"
)

// WebCredential binds a remotely verified identity to exactly one tenant. Browser
// credentials expire even if their in-memory console session is lost on restart.
func (s *Store) WebCredential(ctx context.Context, user string) (Credential, error) {
	if !security.ValidID(user) {
		return Credential{}, errors.New("invalid identity")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 17))`, user); err != nil {
		return Credential{}, err
	}
	var tenant string
	err = tx.QueryRow(ctx, `SELECT tenant_id FROM web_accounts WHERE auth_user_id=$1`, user).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		tenant = security.ID()
		if _, err = tx.Exec(ctx, `INSERT INTO tenants(id,name) VALUES($1,'My workspace')`, tenant); err != nil {
			return Credential{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO web_accounts(auth_user_id,tenant_id) VALUES($1,$2)`, user, tenant); err != nil {
			return Credential{}, err
		}
	} else if err != nil {
		return Credential{}, err
	}
	var active string
	if err = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR UPDATE`, tenant).Scan(&active); err != nil {
		return Credential{}, errors.New("workspace unavailable")
	}
	// Expired browser credentials are expendable, unlike user-created integration keys.
	if _, err = tx.Exec(ctx, `DELETE FROM api_keys WHERE tenant_id=$1 AND expires_at IS NOT NULL AND expires_at<=now()`, tenant); err != nil {
		return Credential{}, err
	}
	c, err := insertKey(ctx, tx, tenant)
	if err != nil {
		return c, err
	}
	if _, err = tx.Exec(ctx, `UPDATE api_keys SET expires_at=now()+interval '30 minutes' WHERE id=$1`, c.KeyID); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s *Store) DeleteBrowserKey(ctx context.Context, key string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM api_keys WHERE key_hash=$1 AND expires_at IS NOT NULL`, security.Hash(key))
	return err
}

// MintWebKey requires a currently active browser credential and returns an ordinary
// integration key once. It never accepts a tenant identifier from browser input.
func (s *Store) MintWebKey(ctx context.Context, token string) (Credential, error) {
	tenant, err := s.Authenticate(ctx, token)
	if err != nil {
		return Credential{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer tx.Rollback(ctx)
	var active string
	if err = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 AND disabled_at IS NULL FOR UPDATE`, tenant).Scan(&active); err != nil {
		return Credential{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE tenant_id=$1 AND expires_at IS NULL AND revoked_at IS NULL`, tenant).Scan(&count); err != nil {
		return Credential{}, err
	}
	if count >= 10 {
		return Credential{}, errors.New("API key limit reached")
	}
	c, err := insertKey(ctx, tx, tenant)
	if err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

type WebKey struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
}

func (s *Store) WebKeys(ctx context.Context, token string) ([]WebKey, error) {
	tenant, err := s.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id,key_prefix FROM api_keys WHERE tenant_id=$1 AND expires_at IS NULL AND revoked_at IS NULL ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WebKey{}
	for rows.Next() {
		var k WebKey
		if err = rows.Scan(&k.ID, &k.Prefix); err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, rows.Err()
}
func (s *Store) RevokeWebKey(ctx context.Context, token, id string) error {
	tenant, err := s.Authenticate(ctx, token)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE tenant_id=$1 AND id=$2 AND expires_at IS NULL`, tenant, id)
	return err
}
func (s *Store) RevokeWebSessions(ctx context.Context, user string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM api_keys WHERE tenant_id=(SELECT tenant_id FROM web_accounts WHERE auth_user_id=$1) AND expires_at IS NOT NULL`, user)
	return err
}
