package store

// DB integration tests for cursor-paginated listings. They run against
// DATABASE_URL and skip otherwise.

import (
	"context"
	"errors"
	"testing"
)

func seedOrder(t *testing.T, repo *OrderRepository, merchant string, key string, amount int64) Order {
	t.Helper()

	order, _, err := repo.CreateIdempotent(context.Background(), Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     amount,
		Currency:   "INR",
		Status:     "CREATED",
	}, key, "lhash-"+key)
	if err != nil {
		t.Fatalf("seed order %s: %v", key, err)
	}

	return order
}

func TestListOrdersPages(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]

	var ids []string
	for i := 0; i < 5; i++ {
		o := seedOrder(t, repo, merchant, "list-order-key-"+string(rune('a'+i)), 1000+int64(i))
		ids = append(ids, o.ID)
	}

	// Newest first: last created comes first.
	page1, more1, err := repo.ListOrders(ctx, merchant, 2, "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}

	if len(page1) != 2 || !more1 {
		t.Fatalf("page 1: got %d rows more=%v, want 2 true", len(page1), more1)
	}

	if page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("page 1: wrong order %+v", page1)
	}

	page2, more2, err := repo.ListOrders(ctx, merchant, 2, page1[1].ID)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	if len(page2) != 2 || !more2 {
		t.Fatalf("page 2: got %d rows more=%v, want 2 true", len(page2), more2)
	}

	if page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("page 2: wrong rows %+v", page2)
	}

	page3, more3, err := repo.ListOrders(ctx, merchant, 2, page2[1].ID)
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}

	if len(page3) != 1 || more3 {
		t.Fatalf("page 3: got %d rows more=%v, want 1 false", len(page3), more3)
	}

	if page3[0].ID != ids[0] {
		t.Fatalf("page 3: wrong row %+v", page3)
	}

	// Unknown and foreign cursors are client errors, not empty pages.
	if _, _, err := repo.ListOrders(ctx, merchant, 2, testUUID(t)); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unknown cursor: got %v, want ErrInvalidCursor", err)
	}

	other := seedOrder(t, repo, "other-"+merchant, "list-other-key", 1000)

	if _, _, err := repo.ListOrders(ctx, merchant, 2, other.ID); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("foreign cursor: got %v, want ErrInvalidCursor", err)
	}

	// Other merchants' rows never leak into this merchant's pages.
	all, more, err := repo.ListOrders(ctx, merchant, 100, "")
	if err != nil {
		t.Fatalf("full list: %v", err)
	}

	if len(all) != 5 || more {
		t.Fatalf("full list: got %d rows more=%v, want 5 false", len(all), more)
	}
}

func TestListPaymentsAndRefunds(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]

	order := seedOrder(t, repo, merchant, "list-pay-order-key", 50000)

	payment, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"list-pay-key",
		"lhash-pay",
	)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	if _, err := repo.CapturePayment(ctx, merchant, payment.ID); err != nil {
		t.Fatalf("seed capture: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := repo.CreateRefundIdempotent(
			ctx, payment.ID, testUUID(t), 1000, merchant,
			"list-ref-key-"+string(rune('a'+i)), "lhash-ref",
		); err != nil {
			t.Fatalf("seed refund %d: %v", i, err)
		}
	}

	payments, more, err := repo.ListPayments(ctx, merchant, 20, "")
	if err != nil {
		t.Fatalf("list payments: %v", err)
	}

	if len(payments) != 1 || more || payments[0].ID != payment.ID {
		t.Fatalf("payments: unexpected %+v more=%v", payments, more)
	}

	refunds, more, err := repo.ListRefunds(ctx, merchant, payment.ID, 2, "")
	if err != nil {
		t.Fatalf("refunds page 1: %v", err)
	}

	if len(refunds) != 2 || !more {
		t.Fatalf("refunds page 1: got %d more=%v, want 2 true", len(refunds), more)
	}

	refunds2, more2, err := repo.ListRefunds(
		ctx, merchant, payment.ID, 2, refunds[1].ID,
	)
	if err != nil {
		t.Fatalf("refunds page 2: %v", err)
	}

	if len(refunds2) != 1 || more2 {
		t.Fatalf("refunds page 2: got %d more=%v, want 1 false", len(refunds2), more2)
	}

	// Missing and foreign payments are 404s.
	if _, _, err := repo.ListRefunds(ctx, merchant, testUUID(t), 20, ""); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("unknown payment: got %v, want ErrPaymentNotFound", err)
	}

	foreign := seedOrder(t, repo, "other-"+merchant, "list-foreign-key", 1000)

	fp, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 1000, Method: "card", Status: "AUTHORIZED"},
		foreign.ID,
		"other-"+merchant,
		"list-foreign-pay-key",
		"lhash-foreign-pay",
	)
	if err != nil {
		t.Fatalf("seed foreign payment: %v", err)
	}

	if _, _, err := repo.ListRefunds(ctx, merchant, fp.ID, 20, ""); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("foreign payment: got %v, want ErrPaymentNotFound", err)
	}
}
