package store

// DB integration tests for platform fees, disputes, and balances. They run
// against DATABASE_URL and skip otherwise.

import (
	"context"
	"errors"
	"testing"
)

func seedCaptured(t *testing.T, repo *OrderRepository, merchant string, orderKey string, amount int64) Payment {
	t.Helper()

	ctx := context.Background()

	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     amount,
		Currency:   "INR",
		Status:     "CREATED",
	}, orderKey, "bhash-"+orderKey)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	payment, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: amount, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"pay-"+orderKey,
		"bhash-pay-"+orderKey,
	)
	if err != nil {
		t.Fatalf("seed authorize: %v", err)
	}

	fee := amount*200/10000 + 300

	captured, err := repo.CapturePayment(ctx, merchant, payment.ID, fee)
	if err != nil {
		t.Fatalf("seed capture: %v", err)
	}

	return captured
}

func TestDisputeLostDeducts(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	payment := seedCaptured(t, repo, merchant, "dispute-order-key", 50000)

	// Fee for 50000 paise is 1300; balance starts at 50000 - 1300 = 48700.
	opened, created, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 10000, "fraud",
	)
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	if !created || opened.Status != DisputeOpen {
		t.Fatalf("open: unexpected %+v created=%v", opened, created)
	}

	// Identical reopen replays the OPEN dispute instead of double-holding.
	replay, created, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 10000, "fraud",
	)
	if err != nil {
		t.Fatalf("reopen identical: %v", err)
	}

	if created || replay.ID != opened.ID {
		t.Fatalf("reopen: got %+v created=%v, want original false", replay, created)
	}

	// A differing open while one is outstanding conflicts.
	if _, _, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 5000, "fraud",
	); !errors.Is(err, ErrDisputeNotAllowed) {
		t.Fatalf("second open: got %v, want ErrDisputeNotAllowed", err)
	}

	// Over-amount and wrong-state opens are rejected.
	if _, _, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 40001, "fraud",
	); !errors.Is(err, ErrDisputeNotAllowed) {
		t.Fatalf("over-amount open: got %v, want ErrDisputeNotAllowed", err)
	}

	fetched, err := repo.GetPayment(ctx, merchant, payment.ID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}

	if fetched.DisputedAmount != 10000 {
		t.Fatalf("disputed_amount = %d, want 10000", fetched.DisputedAmount)
	}

	// Losing converts the hold into settled loss.
	lost, err := repo.ResolveDispute(ctx, merchant, opened.ID, OutcomeLost)
	if err != nil {
		t.Fatalf("resolve lost: %v", err)
	}

	if lost.Status != DisputeLost {
		t.Fatalf("resolved status = %q, want LOST", lost.Status)
	}

	fetched, err = repo.GetPayment(ctx, merchant, payment.ID)
	if err != nil {
		t.Fatalf("get payment after loss: %v", err)
	}

	if fetched.DisputedAmount != 0 || fetched.RefundedAmount != 10000 {
		t.Fatalf("after loss: disputed=%d refunded=%d, want 0 10000",
			fetched.DisputedAmount, fetched.RefundedAmount)
	}

	// Resolving twice replays the outcome; balance reflects the loss:
	// 50000 - 10000 refunded - 1300 fee = 38700.
	if _, err := repo.ResolveDispute(ctx, merchant, opened.ID, OutcomeWon); !errors.Is(err, ErrDisputeNotOpen) {
		t.Fatalf("re-resolve: got %v, want ErrDisputeNotOpen", err)
	}

	balances, err := repo.MerchantBalance(ctx, merchant)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}

	if len(balances) != 1 || balances[0].Currency != "INR" || balances[0].Net != 38700 {
		t.Fatalf("balance: unexpected %+v", balances)
	}
}

func TestDisputeWonReleases(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	payment := seedCaptured(t, repo, merchant, "won-order-key", 20000)

	opened, _, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 5000, "duplicate",
	)
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	won, err := repo.ResolveDispute(ctx, merchant, opened.ID, OutcomeWon)
	if err != nil {
		t.Fatalf("resolve won: %v", err)
	}

	if won.Status != DisputeWon {
		t.Fatalf("resolved status = %q, want WON", won.Status)
	}

	fetched, err := repo.GetPayment(ctx, merchant, payment.ID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}

	if fetched.DisputedAmount != 0 || fetched.RefundedAmount != 0 {
		t.Fatalf("after win: disputed=%d refunded=%d, want 0 0",
			fetched.DisputedAmount, fetched.RefundedAmount)
	}

	// Fee for 20000 paise is 700; a won dispute leaves 20000 - 700 = 19300.
	balances, err := repo.MerchantBalance(ctx, merchant)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}

	if len(balances) != 1 || balances[0].Net != 19300 {
		t.Fatalf("balance: unexpected %+v", balances)
	}

	// A new dispute can follow a resolved one (only one OPEN at a time).
	second, created, err := repo.OpenDispute(
		ctx, merchant, payment.ID, testUUID(t), 3000, "other",
	)
	if err != nil {
		t.Fatalf("second dispute: %v", err)
	}

	if !created || second.Status != DisputeOpen {
		t.Fatalf("second dispute: unexpected %+v created=%v", second, created)
	}
}

func TestDisputeRejects(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	// Disputing a merely authorized hold is rejected: nothing settled yet.
	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "reject-dispute-order-key", "bhash-reject")
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	held, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"reject-dispute-pay-key",
		"bhash-reject-pay",
	)
	if err != nil {
		t.Fatalf("seed authorize: %v", err)
	}

	if _, _, err := repo.OpenDispute(
		ctx, merchant, held.ID, testUUID(t), 1000, "fraud",
	); !errors.Is(err, ErrDisputeNotAllowed) {
		t.Fatalf("dispute on hold: got %v, want ErrDisputeNotAllowed", err)
	}

	if _, _, err := repo.OpenDispute(
		ctx, merchant, testUUID(t), testUUID(t), 1000, "fraud",
	); !errors.Is(err, ErrDisputeNotAllowed) {
		t.Fatalf("dispute unknown payment: got %v, want ErrDisputeNotAllowed", err)
	}

	if _, err := repo.GetDispute(ctx, merchant, testUUID(t)); !errors.Is(err, ErrDisputeNotFound) {
		t.Fatalf("get unknown dispute: got %v, want ErrDisputeNotFound", err)
	}

	if _, err := repo.ResolveDispute(ctx, merchant, testUUID(t), OutcomeWon); !errors.Is(err, ErrDisputeNotFound) {
		t.Fatalf("resolve unknown: got %v, want ErrDisputeNotFound", err)
	}
}
