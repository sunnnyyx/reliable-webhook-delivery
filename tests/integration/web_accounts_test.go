//go:build integration

package integration

import (
	"context"
	"relayforge/internal/security"
	"sync"
	"testing"
)

func TestWebAccountsIsolationExpiryAndConcurrentProvision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := security.ID()
	var wg sync.WaitGroup
	tenants := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := s.WebCredential(ctx, id)
			if e != nil {
				t.Error(e)
				return
			}
			tenants <- c.TenantID
		}()
	}
	wg.Wait()
	close(tenants)
	var first string
	for tenant := range tenants {
		if first == "" {
			first = tenant
		}
		if first != tenant {
			t.Fatal("identity created multiple tenants")
		}
	}
	c, e := s.WebCredential(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, c.APIKey); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE api_keys SET expires_at=now()-interval '1 second' WHERE id=$1`, c.KeyID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, c.APIKey); e == nil {
		t.Fatal("expired browser key accepted")
	}
	c, e = s.WebCredential(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	key, e := s.MintWebKey(ctx, c.APIKey)
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.WebCredential(ctx, security.ID())
	if e != nil {
		t.Fatal(e)
	}
	keys, e := s.WebKeys(ctx, other.APIKey)
	if e != nil || len(keys) != 0 {
		t.Fatal("integration keys leaked between users")
	}
	if e = s.RevokeWebKey(ctx, other.APIKey, key.KeyID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, key.APIKey); e != nil {
		t.Fatal("cross-tenant revocation succeeded")
	}
	if e = s.RevokeWebSessions(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, c.APIKey); e == nil {
		t.Fatal("reset browser key remains active")
	}
	if _, e = s.Authenticate(ctx, key.APIKey); e != nil {
		t.Fatal("reset revoked integration key")
	}
	if e = s.DisableTenant(ctx, first); e != nil {
		t.Fatal(e)
	}
	if _, e = s.WebCredential(ctx, id); e == nil {
		t.Fatal("login re-enabled disabled tenant")
	}
}
