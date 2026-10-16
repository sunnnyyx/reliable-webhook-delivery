//go:build integration

package integration

import (
	"context"
	"errors"
	"relayforge/internal/delivery"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"testing"
	"time"
)

// Exercise both sides of the bounded-count threshold through real acceptance;
// a fan-out larger than the entire cap must not leave a reservation or partial work.
func TestBoundedBacklogExactCapacityAndOversizeFanout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Bounded count")
	targets(t, s, c.TenantID, "order.created")
	targets(t, s, c.TenantID, "order.created")
	s.MaxTenantBacklog = 1
	if _, e := s.Accept(ctx, c.TenantID, "oversize", []byte(orderBody)); !errors.Is(e, postgres.ErrBacklog) {
		t.Fatalf("oversize result: %v", e)
	}
	var n int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM ingestion_idempotency`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("partial reservation %d: %v", n, e)
	}
	s.MaxTenantBacklog = 2
	receipt, e := s.Accept(ctx, c.TenantID, "exact", []byte(orderBody))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Accept(ctx, c.TenantID, "full", []byte(orderBody)); !errors.Is(e, postgres.ErrBacklog) {
		t.Fatalf("full result: %v", e)
	}
	if duplicate, e := s.Accept(ctx, c.TenantID, "exact", []byte(orderBody)); e != nil || !duplicate.Existing || duplicate.EventID != receipt.EventID {
		t.Fatalf("receipt changed: %v", e)
	}
	for range 2 {
		job, e := s.Claim(ctx, security.ID(), time.Minute)
		if e != nil || job == nil {
			t.Fatalf("claim: %v", e)
		}
		if ok, e := s.Complete(ctx, *job, delivery.Classify(204)); e != nil || !ok {
			t.Fatalf("completion: %v", e)
		}
	}
	if _, e = s.Accept(ctx, c.TenantID, "freed", []byte(orderBody)); e != nil {
		t.Fatalf("freed capacity rejected: %v", e)
	}
}
