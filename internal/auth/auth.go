// Package auth issues and checks secret API keys. Only a key's SHA-256 is
// stored (FR-1), so a database leak exposes no usable key.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/ids"
)

// KeyPrefix marks a secret test-mode key.
const KeyPrefix = "sk_test_"

var (
	// ErrUnknownKey means no key with that hash exists.
	ErrUnknownKey = errors.New("unknown api key")

	// ErrExpiredKey means the key belongs to a demo session that has ended.
	ErrExpiredKey = errors.New("api key expired")
)

// Key is what a valid secret key resolves to.
type Key struct {
	MerchantID string
	Operator   bool
	Session    string // the demo session of a sandbox merchant, else empty
}

// Hash is the only form of a key that is ever stored.
func Hash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// NewKey returns a fresh secret key: the prefix and 32 random characters,
// about 190 bits.
func NewKey() string {
	return KeyPrefix + ids.Random(32)
}

// Lookup resolves a raw key. A sandbox merchant's key stops working when
// its demo session ends, judged by the database's clock, the one that set
// the expiry.
func Lookup(ctx context.Context, db *pgxpool.Pool, raw string) (Key, error) {
	var (
		k       Key
		session *string
		expired bool
	)
	err := db.QueryRow(ctx, `SELECT k.merchant_id, k.operator, s.id, coalesce(s.expires_at <= now(), false)
		FROM api_keys k LEFT JOIN demo_sessions s ON s.merchant_id = k.merchant_id
		WHERE k.key_hash = $1`, Hash(raw)).Scan(&k.MerchantID, &k.Operator, &session, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrUnknownKey
	}
	if err != nil {
		return Key{}, fmt.Errorf("look up api key: %w", err)
	}
	if expired {
		return Key{}, ErrExpiredKey
	}
	if session != nil {
		k.Session = *session
	}
	return k, nil
}

// CreateMerchant makes a merchant and its first secret key. The raw key is
// returned once and never again.
func CreateMerchant(ctx context.Context, db *pgxpool.Pool, name string) (merchantID, rawKey string, err error) {
	merchantID, rawKey = ids.New(ids.Merchant), NewKey()
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO merchants (id, name) VALUES ($1, $2)`, merchantID, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO api_keys (key_hash, merchant_id) VALUES ($1, $2)`, Hash(rawKey), merchantID)
		return err
	})
	if err != nil {
		return "", "", fmt.Errorf("create merchant: %w", err)
	}
	return merchantID, rawKey, nil
}
