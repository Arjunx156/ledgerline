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

// ErrUnknownKey means no key with that hash exists.
var ErrUnknownKey = errors.New("unknown api key")

// Key is what a valid secret key resolves to.
type Key struct {
	MerchantID string
	Operator   bool
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

// Lookup resolves a raw key.
func Lookup(ctx context.Context, db *pgxpool.Pool, raw string) (Key, error) {
	var k Key
	err := db.QueryRow(ctx, `SELECT merchant_id, operator FROM api_keys WHERE key_hash = $1`, Hash(raw)).
		Scan(&k.MerchantID, &k.Operator)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrUnknownKey
	}
	if err != nil {
		return Key{}, fmt.Errorf("look up api key: %w", err)
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
