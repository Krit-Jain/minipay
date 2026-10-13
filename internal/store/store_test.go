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

// testPayment builds a well-formed authorization fixture: the store CHECKs
// method values and only flips orders for AUTHORIZED payments, so fixtures
// must carry both fields to exercise the happy path.
func testPayment(t *testing.T, amount int64) Payment {
	t.Helper()

	return Payment{
		ID:     testUUID(t),
		Amount: amount,
		Method: "card",
		Status: "AUTHORIZED",
	}
}

func TestCreateIdempotentReplayAndConflict(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)
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
	cleanupMerchant(t, repo, "test-merchant")

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

func TestCreateRefundIdempotentReplayAndConflict(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)
	key := "test-refund-key"

	orderID := testUUID(t)
	paymentID := testUUID(t)

	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (id, merchant_id, amount, currency, status)
		 VALUES ($1, $2, 50000, 'INR', 'CREATED')`,
		orderID,
		merchant,
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

	first, created, err := repo.CreateRefundIdempotent(
		ctx, paymentID, testUUID(t), 10000, merchant, key, "rhash-aaa",
	)
	if err != nil {
		t.Fatalf("initial refund: %v", err)
	}

	if !created {
		t.Fatal("initial refund: created = false, want true")
	}

	// Same key + same hash replays the original refund, no second reserve.
	replay, created, err := repo.CreateRefundIdempotent(
		ctx, paymentID, testUUID(t), 10000, merchant, key, "rhash-aaa",
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if created {
		t.Fatal("replay: created = true, want false")
	}

	if replay.ID != first.ID {
		t.Fatalf("replay: id = %q, want original %q", replay.ID, first.ID)
	}

	var refundCount, refunded int64

	if err := pool.QueryRow(ctx,
		`SELECT count(*), (SELECT refunded_amount FROM payments WHERE id = $1)
		 FROM refunds WHERE payment_id = $1`,
		paymentID,
	).Scan(&refundCount, &refunded); err != nil {
		t.Fatalf("accounting check: %v", err)
	}

	if refundCount != 1 {
		t.Fatalf("refund rows = %d, want 1 (replay must not duplicate)", refundCount)
	}

	if refunded != 10000 {
		t.Fatalf("refunded_amount = %d, want 10000 (replay must not re-reserve)", refunded)
	}

	// Same key + different hash is rejected and reserves nothing.
	if _, _, err := repo.CreateRefundIdempotent(
		ctx, paymentID, testUUID(t), 5000, merchant, key, "rhash-bbb",
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: got %v, want ErrIdempotencyConflict", err)
	}

	if err := pool.QueryRow(ctx,
		`SELECT refunded_amount FROM payments WHERE id = $1`,
		paymentID,
	).Scan(&refunded); err != nil {
		t.Fatalf("post-conflict check: %v", err)
	}

	if refunded != 10000 {
		t.Fatalf("refunded_amount after conflict = %d, want 10000", refunded)
	}
}

func TestCreateIdempotentAdoptsOrphan(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)
	key := "test-orphan-key"
	orphanID := testUUID(t)

	// Simulate a crash between the order INSERT and the key linkage: the
	// order row exists with the idempotency key, but the key still points
	// nowhere.
	if _, err := pool.Exec(ctx,
		`INSERT INTO idempotency_keys (merchant_id, key, request_hash)
		 VALUES ($1, $2, 'hash-orphan')`,
		merchant,
		key,
	); err != nil {
		t.Fatalf("fixture key: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (id, merchant_id, amount, currency, status, idempotency_key)
		 VALUES ($1, $2, 25000, 'INR', 'CREATED', $3)`,
		orphanID,
		merchant,
		key,
	); err != nil {
		t.Fatalf("fixture orphan order: %v", err)
	}

	// The retry carries a fresh candidate ID, which must be ignored in
	// favour of the orphan — no duplicate order may be created.
	candidate := Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     25000,
		Currency:   "INR",
		Status:     "CREATED",
	}

	got, created, err := repo.CreateIdempotent(ctx, candidate, key, "hash-orphan")
	if err != nil {
		t.Fatalf("orphan replay: %v", err)
	}

	if created {
		t.Fatal("orphan replay: created = true, want false")
	}

	if got.ID != orphanID {
		t.Fatalf("orphan replay: id = %q, want orphan %q", got.ID, orphanID)
	}

	var orderCount int64

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE merchant_id = $1`,
		merchant,
	).Scan(&orderCount); err != nil {
		t.Fatalf("order count: %v", err)
	}

	if orderCount != 1 {
		t.Fatalf("orders for merchant = %d, want 1 (no duplicate)", orderCount)
	}
}
