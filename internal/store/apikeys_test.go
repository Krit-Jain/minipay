package store

// DB integration tests for API-key issuance and authentication. They run
// against DATABASE_URL and skip otherwise.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAPIKeyIssueAndAuthenticate(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]

	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx,
			`DELETE FROM api_keys WHERE merchant_id = $1`, merchant)
		_, _ = pool.Exec(ctx,
			`DELETE FROM merchants WHERE id = $1`, merchant)
	})

	if err := repo.EnsureMerchant(ctx, merchant, "Test Merchant"); err != nil {
		t.Fatalf("ensure merchant: %v", err)
	}

	// Ensure is idempotent.
	if err := repo.EnsureMerchant(ctx, merchant, "Test Merchant"); err != nil {
		t.Fatalf("ensure merchant twice: %v", err)
	}

	raw, err := repo.IssueAPIKey(ctx, merchant)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}

	if !strings.HasPrefix(raw, "sk_test_") {
		t.Fatalf("key %q lacks the sk_test_ prefix", raw)
	}

	// The raw secret must never be stored: only its hash is persisted.
	var hash, prefix string

	if err := pool.QueryRow(ctx,
		`SELECT key_hash, prefix FROM api_keys WHERE merchant_id = $1`,
		merchant,
	).Scan(&hash, &prefix); err != nil {
		t.Fatalf("read stored key: %v", err)
	}

	if hash == raw {
		t.Fatal("raw key stored in plaintext")
	}

	if prefix != raw[:12] {
		t.Fatalf("prefix = %q, want %q", prefix, raw[:12])
	}

	got, err := repo.Authenticate(ctx, raw)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if got != merchant {
		t.Fatalf("merchant = %q, want %q", got, merchant)
	}

	// Unknown, empty, and revoked keys all fail identically.
	if _, err := repo.Authenticate(ctx, "sk_test_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("unknown key: got %v, want ErrInvalidAPIKey", err)
	}

	if _, err := repo.Authenticate(ctx, ""); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("empty key: got %v, want ErrInvalidAPIKey", err)
	}

	if err := repo.RevokeAPIKey(ctx, merchant, raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := repo.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked key: got %v, want ErrInvalidAPIKey", err)
	}

	if err := repo.RevokeAPIKey(ctx, merchant, raw); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("double revoke: got %v, want ErrInvalidAPIKey", err)
	}
}
