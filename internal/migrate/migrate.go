package migrate

import (
	"context"
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"relayforge/migrations"
)

func Provider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
}
func Run(ctx context.Context, url, direction string) error {
	db, e := sql.Open("pgx", url)
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	p, e := Provider(db)
	if e != nil {
		return e
	}
	if direction == "up" {
		_, e = p.Up(ctx)
	} else {
		_, e = p.Down(ctx)
	}
	return e
}
