package store

// DB integration tests for the transactional guarantees the API relies on:
// idempotent order replay, idempotency conflict, and atomic refunds.
//
// They run against the database in DATABASE_URL and skip otherwise, so
// `go test ./...` stays green without a database. Each test uses a unique
// merchant/key namespace, so parallel runs and leftover rows cannot collide.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set, skipping DB integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	return pool
}

func testUUID(t *testing.T) string {
	t.Helper()

	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
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
	)
}

func TestCreateIdempotentReplayAndConflict(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	key := "test-key-replay"

	first := Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}

	got, created, err := repo.CreateIdempotent(ctx, first, key, "hash-aaa")
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}

	if !created {
		t.Fatal("initial create: created = false, want true")
	}

	if got.ID != first.ID {
		t.Fatalf("initial create: id = %q, want %q", got.ID, first.ID)
	}

	// Same key + same hash must replay the original order, not insert.
	replay := Order{
		ID:         testUUID(t), // different candidate ID must be ignored
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}

	got, created, err = repo.CreateIdempotent(ctx, replay, key, "hash-aaa")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if created {
		t.Fatal("replay: created = true, want false")
	}

	if got.ID != first.ID {
		t.Fatalf("replay: id = %q, want original %q", got.ID, first.ID)
	}

	// Same key + different hash must be rejected.
	other := Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     99999,
		Currency:   "INR",
		Status:     "CREATED",
	}

	if _, _, err := repo.CreateIdempotent(ctx, other, key, "hash-bbb"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateRefundAtomicity(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()

	orderID := testUUID(t)
	paymentID := testUUID(t)

	// Minimal order + SUCCEEDED payment fixture. order_id is NOT NULL on
	// payments, so the order row must exist first.
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (id, merchant_id, amount, currency, status)
		 VALUES ($1, 'test-merchant', 50000, 'INR', 'CREATED')`,
		orderID,
	); err != nil {
		t.Fatalf("fixture order: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, order_id, amount, currency, status)
		 VALUES ($1, $2, 50000, 'INR', 'SUCCEEDED')`,
		paymentID,
		orderID,
	); err != nil {
		t.Fatalf("fixture payment: %v", err)
	}

	// Partial refund succeeds and reserves the balance.
	refund, err := repo.CreateRefund(ctx, paymentID, testUUID(t), 10000)
	if err != nil {
		t.Fatalf("partial refund: %v", err)
	}

	if refund.Amount != 10000 || refund.PaymentID != paymentID {
		t.Fatalf("partial refund: unexpected record %+v", refund)
	}

	// Over-refund (40000 remaining, asking 40001) must be rejected.
	if _, err := repo.CreateRefund(ctx, paymentID, testUUID(t), 40001); !errors.Is(err, ErrRefundNotAllowed) {
		t.Fatalf("over-refund: got %v, want ErrRefundNotAllowed", err)
	}

	// Exact remainder succeeds.
	if _, err := repo.CreateRefund(ctx, paymentID, testUUID(t), 40000); err != nil {
		t.Fatalf("remainder refund: %v", err)
	}

	// Fully refunded: any further refund is rejected.
	if _, err := repo.CreateRefund(ctx, paymentID, testUUID(t), 1); !errors.Is(err, ErrRefundNotAllowed) {
		t.Fatalf("exhausted refund: got %v, want ErrRefundNotAllowed", err)
	}

	// Unknown payment is rejected, not inserted.
	if _, err := repo.CreateRefund(ctx, testUUID(t), testUUID(t), 100); !errors.Is(err, ErrRefundNotAllowed) {
		t.Fatalf("unknown payment: got %v, want ErrRefundNotAllowed", err)
	}
}
