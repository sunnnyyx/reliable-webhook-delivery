//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
	"relayforge/internal/migrate"
	"testing"
	"time"
)

func TestMigrationRoundTrip(t *testing.T) {
	url := os.Getenv("INTEGRATION_DATABASE_URL")
	if url == "" {
		t.Fatal("INTEGRATION_DATABASE_URL is required")
	}
	db, e := sql.Open("pgx", url)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Use an isolated schema so the test never drops application tables.
	var suffix [12]byte
	if _, e = rand.Read(suffix[:]); e != nil {
		t.Fatal(e)
	}
	schema := "relayforge_migration_test_" + hex.EncodeToString(suffix[:])
	db.SetMaxOpenConns(1)
	if _, e = db.ExecContext(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	defer db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	if _, e = db.ExecContext(ctx, `SET search_path TO `+schema); e != nil {
		t.Fatal(e)
	}
	p, e := migrate.Provider(db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal("repeat up:", e)
	}
	var name string
	if e = db.QueryRowContext(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name='tenants' AND column_name='id'", schema).Scan(&name); e != nil {
		t.Fatal(e)
	}
	if _, e = p.DownTo(ctx, 0); e != nil {
		t.Fatal(e)
	}
	var exists bool
	if e = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='tenants')", schema).Scan(&exists); e != nil || exists {
		t.Fatalf("table after down: %v %v", exists, e)
	}
	if _, e = p.Up(ctx); e != nil {
		t.Fatal("reapply:", e)
	}
}
