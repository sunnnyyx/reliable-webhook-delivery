package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"relayforge/internal/events"
	"time"
)

var ErrEventConflict = errors.New("event identity reused with different bytes")

type Envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}
type Processor interface {
	Process(context.Context, string, Envelope, []byte) (bool, error)
}

// Postgres is receiver-owned storage. Source is trusted deployment configuration,
// binding one sender tenant/source and its accepted signing keys to this namespace.
type Postgres struct {
	DB     *pgxpool.Pool
	Source string
}

func (p *Postgres) Process(ctx context.Context, deliveryID string, event Envelope, raw []byte) (bool, error) {
	if p.DB == nil || !events.ValidType(p.Source) {
		return false, errors.New("invalid receiver storage configuration")
	}
	tx, e := p.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	hash := events.Hash(raw)
	tag, e := tx.Exec(ctx, `INSERT INTO receiver_processed_events(source,event_id,body_sha256,first_delivery_id) VALUES($1,$2,$3,$4) ON CONFLICT(source,event_id) DO NOTHING`, p.Source, event.ID, hash, deliveryID)
	if e != nil {
		return false, e
	}
	duplicate := tag.RowsAffected() == 0
	if duplicate {
		// A fresh statement sees the winner after the unique constraint waited for commit.
		var stored []byte
		e = tx.QueryRow(ctx, `SELECT body_sha256 FROM receiver_processed_events WHERE source=$1 AND event_id=$2 FOR SHARE`, p.Source, event.ID).Scan(&stored)
		if e != nil {
			return false, e
		}
		if !bytes.Equal(stored, hash) {
			return false, ErrEventConflict
		}
	} else {
		_, e = tx.Exec(ctx, `INSERT INTO receiver_business_events(source,event_id,event_type) VALUES($1,$2,$3)`, p.Source, event.ID, event.Type)
		if e != nil {
			return false, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return false, e
	}
	return duplicate, nil
}

func (p *Postgres) Ready(ctx context.Context) error {
	var ready bool
	if e := p.DB.QueryRow(ctx, "SELECT to_regclass('receiver_processed_events') IS NOT NULL AND to_regclass('receiver_business_events') IS NOT NULL").Scan(&ready); e != nil {
		return e
	}
	if !ready {
		return errors.New("receiver schema unavailable")
	}
	return nil
}
