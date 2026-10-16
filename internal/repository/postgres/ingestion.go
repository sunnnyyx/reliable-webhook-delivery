package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"relayforge/internal/events"
	"relayforge/internal/security"
	"time"
)

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}
func (s *Store) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return security.ID()
}
func (s *Store) accept(ctx context.Context, tenant, key string, raw []byte) (events.Accepted, error) {
	if e := events.ValidateKey(key); e != nil {
		return events.Accepted{}, ValidationError{e}
	}
	input, e := events.Parse(raw)
	if e != nil {
		return events.Accepted{}, ValidationError{e}
	}
	hash := events.Hash(raw)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return events.Accepted{}, e
	}
	defer tx.Rollback(ctx)
	var id string
	var now time.Time
	for {
		id = s.newID()
		now = s.now()
		// Unique-key conflict waits for an uncommitted owner. A fresh SQL statement
		// then sees that owner's committed response under READ COMMITTED.
		tag, e := tx.Exec(ctx, `INSERT INTO ingestion_idempotency(tenant_id,idempotency_key,request_hash,event_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, tenant, key, hash, id, now, now.Add(7*24*time.Hour))
		if e != nil {
			return events.Accepted{}, e
		}
		if tag.RowsAffected() == 1 {
			break
		}
		var storedHash, body []byte
		var storedID string
		var status int
		var expires time.Time
		e = tx.QueryRow(ctx, `SELECT request_hash,event_id,response_status,response_body,expires_at FROM ingestion_idempotency WHERE tenant_id=$1 AND idempotency_key=$2 FOR UPDATE`, tenant, key).Scan(&storedHash, &storedID, &status, &body, &expires)
		if errors.Is(e, pgx.ErrNoRows) {
			continue
		}
		if e != nil {
			return events.Accepted{}, e
		}
		if !expires.After(s.now()) {
			if _, e = tx.Exec(ctx, "DELETE FROM ingestion_idempotency WHERE tenant_id=$1 AND idempotency_key=$2", tenant, key); e != nil {
				return events.Accepted{}, e
			}
			continue
		}
		if !bytes.Equal(hash, storedHash) {
			return events.Accepted{}, events.ErrIdempotencyConflict
		}
		if e = tx.Commit(ctx); e != nil {
			return events.Accepted{}, e
		}
		return events.Accepted{EventID: storedID, Status: status, Body: body, Existing: true}, nil
	}
	if e = lockAdmission(ctx, tx, tenant); e != nil {
		return events.Accepted{}, e
	}
	now = s.now()
	// One matching statement snapshots subscriptions and URLs. Shared row locks
	// coordinate with update/delete; delete subsequently cancels committed work.
	rows, e := tx.Query(ctx, `SELECT id,url FROM endpoints WHERE tenant_id=$1 AND enabled AND deleted_at IS NULL AND ($2=ANY(subscribed_event_types) OR '*'=ANY(subscribed_event_types)) ORDER BY id FOR SHARE`, tenant, input.Type)
	if e != nil {
		return events.Accepted{}, e
	}
	type target struct{ id, url string }
	targets := []target{}
	for rows.Next() {
		var t target
		if e = rows.Scan(&t.id, &t.url); e != nil {
			rows.Close()
			return events.Accepted{}, e
		}
		targets = append(targets, t)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return events.Accepted{}, e
	}
	if e = s.checkBacklog(ctx, tx, tenant, len(targets)); e != nil {
		return events.Accepted{}, e
	}
	body := events.Envelope(id, now, input)
	if _, e = tx.Exec(ctx, `INSERT INTO events(id,tenant_id,event_type,outgoing_body,body_sha256,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, tenant, input.Type, body, events.Hash(body), now, now.Add(30*24*time.Hour)); e != nil {
		return events.Accepted{}, e
	}
	if e = s.saveTrace(ctx, tx, tenant, id); e != nil {
		return events.Accepted{}, e
	}
	for _, t := range targets {
		if _, e = tx.Exec(ctx, `INSERT INTO deliveries(id,tenant_id,event_id,endpoint_id,url_snapshot,next_attempt_at,cycle_started_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6,$6,$6)`, s.newID(), tenant, id, t.id, t.url, now); e != nil {
			return events.Accepted{}, e
		}
	}
	response, e := json.Marshal(events.Acceptance{EventID: id, DeliveryCount: len(targets), AcceptedAt: now})
	if e != nil {
		return events.Accepted{}, e
	}
	if _, e = tx.Exec(ctx, `UPDATE ingestion_idempotency SET response_status=202,response_body=$3,created_at=$4,expires_at=$5 WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, key, response, now, now.Add(7*24*time.Hour)); e != nil {
		return events.Accepted{}, e
	}
	// No caller may acknowledge until both deferred constraints and commit succeed.
	if e = tx.Commit(ctx); e != nil {
		return events.Accepted{}, e
	}
	return events.Accepted{EventID: id, Status: 202, Body: response}, nil
}
