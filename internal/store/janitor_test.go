package store

// DB integration tests for the janitor: hold expiry, key collection, and
// single-winner election. They run against DATABASE_URL and skip otherwise.

import (
	"context"
	"testing"
	"time"
)

func TestExpireHolds(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	stale := seedCaptured(t, repo, merchant, "janitor-stale-order", 50000)

	// seedCaptured settles the payment; reopen it as a hold for this test.
	if _, err := pool.Exec(ctx,
		`UPDATE payments SET status = 'AUTHORIZED', fee = 0 WHERE id = $1`,
		stale.ID,
	); err != nil {
		t.Fatalf("reopen hold: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE orders SET status = 'AUTHORIZED' WHERE id = $1`,
		stale.OrderID,
	); err != nil {
		t.Fatalf("reopen order: %v", err)
	}

	fresh := seedCaptured(t, repo, merchant, "janitor-fresh-order", 10000)

	if _, err := pool.Exec(ctx,
		`UPDATE payments SET status = 'AUTHORIZED', fee = 0 WHERE id = $1`,
		fresh.ID,
	); err != nil {
		t.Fatalf("reopen fresh hold: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE orders SET status = 'AUTHORIZED' WHERE id = $1`,
		fresh.OrderID,
	); err != nil {
		t.Fatalf("reopen fresh order: %v", err)
	}

	failed := seedCaptured(t, repo, merchant, "janitor-failed-order", 20000)

	if _, err := pool.Exec(ctx,
		`UPDATE payments SET status = 'FAILED' WHERE id = $1`,
		failed.ID,
	); err != nil {
		t.Fatalf("fail payment: %v", err)
	}

	// Age only the stale hold past the TTL.
	if _, err := pool.Exec(ctx,
		`UPDATE payments SET created_at = now() - make_interval(hours => 2) WHERE id = $1`,
		stale.ID,
	); err != nil {
		t.Fatalf("age hold: %v", err)
	}

	expired, _, elected, err := repo.RunJanitor(ctx, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}

	if !elected || expired != 1 {
		t.Fatalf("janitor: elected=%v expired=%d, want true 1", elected, expired)
	}

	got, err := repo.GetPayment(ctx, merchant, stale.ID)
	if err != nil {
		t.Fatalf("get swept payment: %v", err)
	}

	if got.Status != "CANCELLED" {
		t.Fatalf("swept status = %q, want CANCELLED", got.Status)
	}

	order, err := repo.GetOrder(ctx, merchant, stale.OrderID)
	if err != nil {
		t.Fatalf("get swept order: %v", err)
	}

	if order.Status != "CREATED" {
		t.Fatalf("swept order = %q, want CREATED (payable again)", order.Status)
	}

	// The sweep emits payment.voided like a merchant void would.
	var voids int

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		WHERE merchant_id = $1 AND type = $2 AND payment_id = $3
	`, merchant, EventPaymentVoided, stale.ID).Scan(&voids)
	if err != nil {
		t.Fatalf("void events: %v", err)
	}

	if voids != 1 {
		t.Fatalf("void events = %d, want 1", voids)
	}

	// Fresh holds and failed payments are untouched; a second run is a no-op.
	for _, id := range []string{fresh.ID, failed.ID} {
		p, err := repo.GetPayment(ctx, merchant, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}

		if id == fresh.ID && p.Status != "AUTHORIZED" {
			t.Fatalf("fresh hold = %q, want AUTHORIZED", p.Status)
		}

		if id == failed.ID && p.Status != "FAILED" {
			t.Fatalf("failed payment = %q, want FAILED", p.Status)
		}
	}

	expired, _, _, err = repo.RunJanitor(ctx, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("second janitor: %v", err)
	}

	if expired != 0 {
		t.Fatalf("second run expired = %d, want 0", expired)
	}
}

func TestPurgeKeys(t *testing.T) {
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
	}, "gc-key", "ghash")
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	// Age the key past the TTL; the money row stays young.
	if _, err := pool.Exec(ctx,
		`UPDATE idempotency_keys SET created_at = now() - make_interval(hours => 2)
		 WHERE merchant_id = $1 AND key = $2`,
		merchant, "gc-key",
	); err != nil {
		t.Fatalf("age key: %v", err)
	}

	_, purged, elected, err := repo.RunJanitor(ctx, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}

	if !elected || purged != 1 {
		t.Fatalf("janitor: elected=%v purged=%d, want true 1", elected, purged)
	}

	// The order (money) survives collection; only the linkage row goes.
	if _, err := repo.GetOrder(ctx, merchant, order.ID); err != nil {
		t.Fatalf("order after GC: %v", err)
	}

	var keys int

	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE merchant_id = $1`,
		merchant,
	).Scan(&keys)
	if err != nil {
		t.Fatalf("count keys: %v", err)
	}

	if keys != 0 {
		t.Fatalf("keys remaining = %d, want 0", keys)
	}
}

func TestJanitorElectsOneWinner(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()

	// Hold the election lock on a separate connection: any janitor pass
	// attempted meanwhile must lose, deterministically — try_lock never
	// succeeds against a held lock, whenever the attempt lands.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer holder.Release()

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, janitorLockKey); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	result := make(chan bool, 1)

	go func() {
		_, _, elected, err := repo.RunJanitor(ctx, time.Hour, 24*time.Hour)
		if err != nil {
			t.Errorf("janitor race: %v", err)
			result <- true

			return
		}

		result <- elected
	}()

	select {
	case elected := <-result:
		if elected {
			t.Fatal("janitor won against a held lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("janitor did not return while the lock was held")
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, janitorLockKey); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	// With the lock free, the same call wins.
	_, _, elected, err := repo.RunJanitor(ctx, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}

	if !elected {
		t.Fatal("janitor lost with the lock free")
	}
}
