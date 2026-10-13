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
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
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

	if first.Status != "AUTHORIZED" {
		t.Fatalf("payment status = %q, want AUTHORIZED", first.Status)
	}

	if first.OrderID != order.ID || first.Currency != "INR" {
		t.Fatalf("payment not linked to order: %+v", first)
	}

	// The order must have flipped to AUTHORIZED in the same transaction.
	got, err := repo.GetOrder(ctx, merchant, order.ID)
	if err != nil {
		t.Fatalf("get order after authorize: %v", err)
	}

	if got.Status != "AUTHORIZED" {
		t.Fatalf("order status = %q, want AUTHORIZED", got.Status)
	}

	// Capturing settles both payment and order, and recapturing replays.
	captured, err := repo.CapturePayment(ctx, merchant, first.ID, 1300)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	if captured.Status != "SUCCEEDED" {
		t.Fatalf("captured status = %q, want SUCCEEDED", captured.Status)
	}

	// 50000 paise at 200bps + 300 fixed = 1300 platform fee.
	if captured.Fee != 1300 {
		t.Fatalf("captured fee = %d, want 1300", captured.Fee)
	}

	recaptured, err := repo.CapturePayment(ctx, merchant, first.ID, 1300)
	if err != nil {
		t.Fatalf("recapture: %v", err)
	}

	if recaptured.ID != first.ID || recaptured.Status != "SUCCEEDED" {
		t.Fatalf("recapture: unexpected record %+v", recaptured)
	}

	got, err = repo.GetOrder(ctx, merchant, order.ID)
	if err != nil {
		t.Fatalf("get order after capture: %v", err)
	}

	if got.Status != "SUCCEEDED" {
		t.Fatalf("order status = %q, want SUCCEEDED", got.Status)
	}

	// Same key + same hash replays the original payment.
	replay, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
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
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
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
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
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
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
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

func TestVoidReleasesOrder(t *testing.T) {
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
	}, "void-order-key", "vhash-order")
	if err != nil {
		t.Fatalf("fixture order: %v", err)
	}

	held, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"void-pay-key",
		"vhash-pay",
	)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	// A held payment cannot be refunded: the money has not moved yet.
	if _, _, err := repo.CreateRefundIdempotent(
		ctx, held.ID, testUUID(t), 10000, merchant, "void-ref-key", "vhash-ref",
	); !errors.Is(err, ErrRefundNotAllowed) {
		t.Fatalf("refund of authorized payment: got %v, want ErrRefundNotAllowed", err)
	}

	voided, err := repo.VoidPayment(ctx, merchant, held.ID)
	if err != nil {
		t.Fatalf("void: %v", err)
	}

	if voided.Status != "CANCELLED" {
		t.Fatalf("voided status = %q, want CANCELLED", voided.Status)
	}

	// Re-voiding replays; capturing a void is rejected so a void can never
	// be silently undone.
	revoided, err := repo.VoidPayment(ctx, merchant, held.ID)
	if err != nil {
		t.Fatalf("re-void: %v", err)
	}

	if revoided.ID != held.ID {
		t.Fatalf("re-void: id = %q, want %q", revoided.ID, held.ID)
	}

	if _, err := repo.CapturePayment(ctx, merchant, held.ID, 1300); !errors.Is(err, ErrPaymentNotCapturable) {
		t.Fatalf("capture after void: got %v, want ErrPaymentNotCapturable", err)
	}

	// The order is payable again with a fresh key.
	retry, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"void-retry-key",
		"vhash-retry",
	)
	if err != nil {
		t.Fatalf("repay after void: %v", err)
	}

	if !created || retry.Status != "AUTHORIZED" {
		t.Fatalf("repay after void: unexpected %+v created=%v", retry, created)
	}

	// And a settled payment cannot be voided: money that moved must be
	// refunded instead.
	if _, err := repo.CapturePayment(ctx, merchant, retry.ID, 1300); err != nil {
		t.Fatalf("capture retry: %v", err)
	}

	if _, err := repo.VoidPayment(ctx, merchant, retry.ID); !errors.Is(err, ErrPaymentNotVoidable) {
		t.Fatalf("void after capture: got %v, want ErrPaymentNotVoidable", err)
	}

	if _, err := repo.CapturePayment(ctx, merchant, testUUID(t), 0); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("capture unknown: got %v, want ErrPaymentNotFound", err)
	}
}

func TestDeclinedPaymentLeavesOrderPayable(t *testing.T) {
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
	}, "decline-order-key", "dhash-order")
	if err != nil {
		t.Fatalf("fixture order: %v", err)
	}

	// A gateway decline is recorded, not thrown away: the FAILED row is the
	// audit trail of the attempt, with the reason attached.
	declined, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{
			ID:          testUUID(t),
			Amount:      50000,
			Method:      "card",
			CardLast4:   "0002",
			FailureCode: "card_declined",
			Status:      "FAILED",
		},
		order.ID,
		merchant,
		"decline-pay-key",
		"dhash-pay",
	)
	if err != nil {
		t.Fatalf("declined payment: %v", err)
	}

	if !created || declined.Status != "FAILED" {
		t.Fatalf("decline: unexpected %+v created=%v", declined, created)
	}

	if declined.FailureCode != "card_declined" || declined.CardLast4 != "0002" {
		t.Fatalf("decline: details lost %+v", declined)
	}

	// The order is untouched and payable again; the failed hold settles
	// into nothing.
	got, err := repo.GetOrder(ctx, merchant, order.ID)
	if err != nil {
		t.Fatalf("get order after decline: %v", err)
	}

	if got.Status != "CREATED" {
		t.Fatalf("order status = %q, want CREATED", got.Status)
	}

	if _, err := repo.CapturePayment(ctx, merchant, declined.ID, 1300); !errors.Is(err, ErrPaymentNotCapturable) {
		t.Fatalf("capture declined: got %v, want ErrPaymentNotCapturable", err)
	}

	retry, created, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"decline-retry-key",
		"dhash-retry",
	)
	if err != nil {
		t.Fatalf("repay after decline: %v", err)
	}

	if !created || retry.Status != "AUTHORIZED" {
		t.Fatalf("repay after decline: unexpected %+v created=%v", retry, created)
	}
}
