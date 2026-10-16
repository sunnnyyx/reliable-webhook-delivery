package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

func Open(ctx context.Context, url string, max int32) (*pgxpool.Pool, error) {
	c, e := pgxpool.ParseConfig(url)
	if e != nil {
		return nil, errors.New("invalid database configuration")
	}
	c.MaxConns = max
	c.ConnConfig.ConnectTimeout = 5 * time.Second
	p, e := pgxpool.NewWithConfig(ctx, c)
	if e != nil {
		return nil, errors.New("database pool initialization failed")
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, errors.New("database connection failed")
	}
	return p, nil
}

// Ping checks the critical sender schema as well as database connectivity. A
// reachable pre-migration database must not advertise application readiness.
func (s *Store) Ping(ctx context.Context) error {
	if e := s.DB.Ping(ctx); e != nil {
		return e
	}
	var ready bool
	e := s.DB.QueryRow(ctx, `SELECT bool_and(to_regclass(name) IS NOT NULL) FROM unnest(ARRAY['tenants','endpoints','events','deliveries','delivery_attempts','delivery_replays','tenant_dispatch_state','event_trace_context']) AS required(name)`).Scan(&ready)
	if e != nil {
		return e
	}
	if !ready {
		return errors.New("critical schema unavailable")
	}
	return nil
}
