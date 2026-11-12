// Package demo is the backend of the public proof console (amendment 3.13):
// sandbox sessions that expire, and the live trace of what each one did,
// stored in Postgres so a stream can follow it from any process. Only
// `ledgerline demo` uses it.
package demo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/ids"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// SessionTTL is how long a sandbox session lives.
const SessionTTL = time.Hour

// ErrNoSession means no session has that id, or it has expired.
var ErrNoSession = errors.New("no live demo session with that id")

// Session is one visitor's sandbox: a merchant of its own, for an hour.
type Session struct {
	ID         string
	MerchantID string
	ExpiresAt  time.Time
}

// CreateSession makes a sandbox merchant, its secret key and a session that
// ends after ttl. The raw key is returned once and never stored.
func CreateSession(ctx context.Context, db *pgxpool.Pool, ttl time.Duration) (Session, string, error) {
	s := Session{ID: ids.New(ids.DemoSession), MerchantID: ids.New(ids.Merchant)}
	key := auth.NewKey()
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO merchants (id, name, sandbox) VALUES ($1, 'Demo sandbox', true)`,
			s.MerchantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO api_keys (key_hash, merchant_id) VALUES ($1, $2)`,
			auth.Hash(key), s.MerchantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO demo_sessions (id, merchant_id, expires_at)
			VALUES ($1, $2, now() + $3::bigint * interval '1 millisecond') RETURNING expires_at`,
			s.ID, s.MerchantID, ttl.Milliseconds()).Scan(&s.ExpiresAt)
	})
	if err != nil {
		return Session{}, "", fmt.Errorf("create demo session: %w", err)
	}
	return s, key, nil
}

// FindSession returns a live session.
func FindSession(ctx context.Context, db *pgxpool.Pool, id string) (Session, error) {
	var s Session
	err := db.QueryRow(ctx, `SELECT id, merchant_id, expires_at FROM demo_sessions
		WHERE id = $1 AND expires_at > now()`, id).Scan(&s.ID, &s.MerchantID, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("find demo session %s: %w", id, err)
	}
	return s, nil
}

// EventsAfter returns up to limit of a session's trace events numbered
// above after, in order.
func EventsAfter(ctx context.Context, db *pgxpool.Pool, session string, after int64, limit int) ([]trace.Event, error) {
	rows, err := db.Query(ctx, `SELECT seq, session, request_id, at, lane, site,
			coalesce(payment_id, ''), coalesce(key, ''), data
		FROM demo_trace_events WHERE session = $1 AND seq > $2 ORDER BY seq LIMIT $3`, session, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read trace of %s: %w", session, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (trace.Event, error) {
		var (
			e  trace.Event
			at time.Time
		)
		err := row.Scan(&e.Seq, &e.Session, &e.RequestID, &at, &e.Lane, &e.Site, &e.PaymentID, &e.Key, &e.Data)
		e.At = at.UnixMilli()
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read trace of %s: %w", session, err)
	}
	return events, nil
}
