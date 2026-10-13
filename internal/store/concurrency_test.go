package store

// Concurrency proof: hammer the exactly-once guarantees with parallel
// goroutines and assert the ledger stays exact. Run with -race in CI.

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestConcurrentSameKeyOrder fires 20 simultaneous creates with one
// idempotency key: exactly one wins, the other 19 replay the same record,
// and only one order row exists afterwards.
func TestConcurrentSameKeyOrder(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	const racers = 20

	type result struct {
		id      string
		created bool
		err     error
	}

	results := make(chan result, racers)
	var wg sync.WaitGroup

	for i := 0; i < racers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			order, created, err := repo.CreateIdempotent(ctx, Order{
				ID:         testUUID(t),
				MerchantID: merchant,
				Amount:     50000,
				Currency:   "INR",
				Status:     "CREATED",
			}, "race-order-key", "rhash-race")

			results <- result{id: order.ID, created: created, err: err}
		}()
	}

	wg.Wait()
	close(results)

	created := 0
	ids := make(map[string]bool)

	for r := range results {
		if r.err != nil {
			t.Fatalf("racer failed: %v", r.err)
		}

		if r.created {
			created++
		}

		ids[r.id] = true
	}

	if created != 1 {
		t.Fatalf("created winners = %d, want exactly 1", created)
	}

	if len(ids) != 1 {
		t.Fatalf("distinct ids = %d, want 1 (all replays agree)", len(ids))
	}

	var count int

	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE merchant_id = $1`,
		merchant,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count orders: %v", err)
	}

	if count != 1 {
		t.Fatalf("order rows = %d, want 1", count)
	}
}

// TestConcurrentOverRefund fires 10 simultaneous 10000 refunds against a
// 50000 payment: exactly 5 win (50000/10000), the rest lose with
// ErrRefundNotAllowed, and the balance lands exactly on zero — never below.
func TestConcurrentOverRefund(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "race-ref-order-key", "rhash-race-ref-order")
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, order_id, amount, currency, status)
		 VALUES ($1, $2, 50000, 'INR', 'SUCCEEDED')`,
		testUUID(t),
		order.ID,
	); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	var paymentID string

	err = pool.QueryRow(ctx,
		`SELECT id::text FROM payments WHERE order_id = $1`,
		order.ID,
	).Scan(&paymentID)
	if err != nil {
		t.Fatalf("payment id: %v", err)
	}

	const racers = 10

	errs := make(chan error, racers)
	var wg sync.WaitGroup

	for i := 0; i < racers; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, err := repo.CreateRefund(
				ctx,
				paymentID,
				testUUID(t),
				10000,
			)
			errs <- err
		}(i)
	}

	wg.Wait()
	close(errs)

	won, rejected := 0, 0

	for err := range errs {
		if err == nil {
			won++
		} else if errors.Is(err, ErrRefundNotAllowed) {
			rejected++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if won != 5 || rejected != 5 {
		t.Fatalf("won=%d rejected=%d, want 5 and 5", won, rejected)
	}

	var refunded int64
	var rows int64

	err = pool.QueryRow(ctx, `
		SELECT refunded_amount,
		       (SELECT count(*) FROM refunds WHERE payment_id = $1)
		FROM payments WHERE id = $1
	`, paymentID).Scan(&refunded, &rows)
	if err != nil {
		t.Fatalf("accounting: %v", err)
	}

	if refunded != 50000 || rows != 5 {
		t.Fatalf("refunded=%d rows=%d, want 50000 and 5", refunded, rows)
	}
}
