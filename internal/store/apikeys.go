package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidAPIKey = errors.New("invalid API key")

// apiKeyPrefix marks test-stage secret keys, Stripe-style. The prefix is
// part of the raw key the merchant sends on every request.
const apiKeyPrefix = "sk_test_"

// hashAPIKey reduces a raw key to its storable form. Only hashes touch the
// database; a leak of api_keys still does not disclose usable secrets.
func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}

func newUUID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	), nil
}

// EnsureMerchant creates the merchant row unless it already exists, so key
// issuance never fails on ordering.
func (r *OrderRepository) EnsureMerchant(
	ctx context.Context,
	id string,
	name string,
) error {
	_, err := r.DB.Exec(ctx, `
		INSERT INTO merchants (id, name)
		VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING
	`, id, name)

	return err
}

// IssueAPIKey mints a raw secret for the merchant and stores only its hash.
// The raw key is returned once and cannot be recovered afterwards.
func (r *OrderRepository) IssueAPIKey(
	ctx context.Context,
	merchantID string,
) (string, error) {
	var secret [24]byte

	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}

	raw := apiKeyPrefix + hex.EncodeToString(secret[:])

	id, err := newUUID()
	if err != nil {
		return "", err
	}

	_, err = r.DB.Exec(ctx, `
		INSERT INTO api_keys (id, merchant_id, key_hash, prefix)
		VALUES ($1, $2, $3, $4)
	`, id, merchantID, hashAPIKey(raw), raw[:12])
	if err != nil {
		return "", err
	}

	return raw, nil
}

// Authenticate resolves a raw key to its merchant, rejecting unknown and
// revoked keys alike with ErrInvalidAPIKey (no oracle for attackers).
func (r *OrderRepository) Authenticate(
	ctx context.Context,
	raw string,
) (string, error) {
	if raw == "" {
		return "", ErrInvalidAPIKey
	}

	var merchantID string

	err := r.DB.QueryRow(ctx, `
		SELECT merchant_id
		FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL
	`, hashAPIKey(raw)).Scan(&merchantID)

	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidAPIKey
	}

	if err != nil {
		return "", err
	}

	return merchantID, nil
}

// RevokeAPIKey soft-revokes the key so in-flight audit trails keep working
// while the secret stops authenticating immediately.
func (r *OrderRepository) RevokeAPIKey(
	ctx context.Context,
	merchantID string,
	raw string,
) error {
	tag, err := r.DB.Exec(ctx, `
		UPDATE api_keys
		SET revoked_at = now()
		WHERE key_hash = $1 AND merchant_id = $2 AND revoked_at IS NULL
	`, hashAPIKey(raw), merchantID)
	if err != nil {
		return err
	}

	if tag.RowsAffected() == 0 {
		return ErrInvalidAPIKey
	}

	return nil
}
