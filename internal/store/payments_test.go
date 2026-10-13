package store

// DB integration tests for payment creation and reads. They run against
// DATABASE_URL and skip otherwise. Each test uses a unique merchant
// namespace so runs cannot collide.

import (
	"context"
	"errors"
	"testing"
)

func TestCreatePaymentLifecycle(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]

	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "pay-order-key", "phash-order")
	if err != nil {
		t.Fatalf("fixture order: %v", err)
	}

	first, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000},
		order.ID,
		merchant,
		"pay-key",
		"phash-aaa",
	)
	if err != nil {
		t.Fatalf("initial payment: %v", err)
	}

	if !created {
		t.Fatal("initial payment: created = false, want true")
	}

	if first.Status != "SUCCEEDED" {
		t.Fatalf("payment status = %q, want SUCCEEDED", first.Status)
	}

	if first.OrderID != order.ID || first.Currency != "INR" {
		t.Fatalf("payment not linked to order: %+v", first)
	}

	// The order must have flipped to SUCCEEDED in the same transaction.
	got, err := repo.GetOrder(ctx, merchant, order.ID)
	if err != nil {
		t.Fatalf("get order after pay: %v", err)
	}

	if got.Status != "SUCCEEDED" {
		t.Fatalf("order status = %q, want SUCCEEDED", got.Status)
	}

	// Same key + same hash replays the original payment.
	replay, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000},
		order.ID,
		merchant,
		"pay-key",
		"phash-aaa",
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

	// Same key + different hash is rejected.
	if _, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000},
		order.ID,
		merchant,
		"pay-key",
		"phash-bbb",
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: got %v, want ErrIdempotencyConflict", err)
	}

	// The payment is readable back, merchant-scoped through its order.
	fetched, err := repo.GetPayment(ctx, merchant, first.ID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}

	if fetched.ID != first.ID || fetched.RefundedAmount != 0 {
		t.Fatalf("unexpected payment record: %+v", fetched)
	}
}

func TestCreatePaymentRejects(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]

	// Unknown order.
	if _, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 100},
		testUUID(t),
		merchant,
		"pay-unknown-key",
		"phash-unknown",
	); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("unknown order: got %v, want ErrOrderNotFound", err)
	}

	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "pay-reject-order-key", "phash-reject-order")
	if err != nil {
		t.Fatalf("fixture order: %v", err)
	}

	// Amount must equal the order amount.
	if _, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 10000},
		order.ID,
		merchant,
		"pay-mismatch-key",
		"phash-mismatch",
	); !errors.Is(err, ErrPaymentAmountMismatch) {
		t.Fatalf("amount mismatch: got %v, want ErrPaymentAmountMismatch", err)
	}

	// A failed validation must leave no payment and no key linkage behind:
	// reusing the same key with the correct amount must succeed fresh.
	payment, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000},
		order.ID,
		merchant,
		"pay-mismatch-key",
		"phash-correct",
	)
	if err != nil {
		t.Fatalf("retry after mismatch: %v", err)
	}

	if !created {
		t.Fatal("retry after mismatch: created = false, want true")
	}

	// A paid order cannot be paid again, even with a fresh key.
	if _, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000},
		order.ID,
		merchant,
		"pay-second-key",
		"phash-second",
	); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Fatalf("second payment: got %v, want ErrOrderAlreadyPaid", err)
	}

	// Reads of missing records are 404s, and payments are invisible to
	// other merchants.
	if _, err := repo.GetOrder(ctx, merchant, testUUID(t)); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("get missing order: got %v, want ErrOrderNotFound", err)
	}

	if _, err := repo.GetPayment(ctx, merchant, testUUID(t)); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("get missing payment: got %v, want ErrPaymentNotFound", err)
	}

	if _, err := repo.GetPayment(ctx, "other-merchant", payment.ID); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("cross-merchant read: got %v, want ErrPaymentNotFound", err)
	}
}
