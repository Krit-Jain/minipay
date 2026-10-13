package store

import (
	"context"
	"time"
)

// Janitor election: one cluster-wide lock so N replicas elect exactly one
// sweeper per tick instead of expiring and purging in parallel. The key is
// arbitrary but fixed — advisory locks live outside tables, in Postgres'
// own lock manager, so no schema is needed for them.
const janitorLockKey = 42001

// RunJanitor performs one maintenance pass: expire stale authorization
// holds, then garbage-collect settled idempotency keys. It returns what it
// did and whether this replica won the election; losers do nothing.
//
// Both jobs are driven by created_at timestamps that already exist, so this
// phase needs no migration: a payment row is born AUTHORIZED (its creation
// is the hold's start), and key rows carry their claim time.
func (r *OrderRepository) RunJanitor(
	ctx context.Context,
	holdTTL time.Duration,
	keyTTL time.Duration,
) (expired int, purged int, elected bool, err error) {
	// Session-scoped lock: hold one pooled connection from acquire to
	// unlock, otherwise the lock could land on (and vanish with) any
	// connection the pool hands out.
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	defer conn.Release()

	var ok bool

	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, janitorLockKey,
	).Scan(&ok); err != nil {
		return 0, 0, false, err
	}

	if !ok {
		return 0, 0, false, nil
	}

	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, janitorLockKey)

	expired, err = r.expireHolds(ctx, holdTTL)
	if err != nil {
		return 0, 0, true, err
	}

	purged, err = r.purgeKeys(ctx, keyTTL)
	if err != nil {
		return expired, 0, true, err
	}

	return expired, purged, true, nil
}

// expiredHold is one authorization the sweeper is about to release.
type expiredHold struct {
	PaymentID string
	OrderID   string
}

// expireHolds voids AUTHORIZED payments older than the TTL and returns
// their orders to CREATED, emitting payment.voided per hold — the same
// transition a merchant void performs, just initiated by time instead of an
// API call. Concurrent captures race safely: whoever holds the row lock
// first wins, and the loser sees the settled state (capture gets 409 on a
// swept hold, the sweeper skips an in-flight capture because its lock
// waits).
func (r *OrderRepository) expireHolds(
	ctx context.Context,
	ttl time.Duration,
) (int, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Batch cap keeps one tick bounded if the sweeper was down for a long
	// while and staleness piled up; leftovers go on the next tick.
	rows, err := tx.Query(ctx, `
		UPDATE payments
		SET status = 'CANCELLED'
		WHERE id IN (
			SELECT id FROM payments
			WHERE status = 'AUTHORIZED'
			  AND created_at < now() - make_interval(secs => $1)
			ORDER BY created_at
			LIMIT 100
		)
		RETURNING id::text, order_id::text
	`, int(ttl.Seconds()))
	if err != nil {
		return 0, err
	}

	var holds []expiredHold

	for rows.Next() {
		var hold expiredHold
		if err := rows.Scan(&hold.PaymentID, &hold.OrderID); err != nil {
			rows.Close()
			return 0, err
		}

		holds = append(holds, hold)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}

	rows.Close()

	for _, hold := range holds {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = 'CREATED' WHERE id = $1
		`, hold.OrderID); err != nil {
			return 0, err
		}

		var payment Payment

		err := tx.QueryRow(ctx, `
			SELECT id::text, order_id::text, amount,
			       currency, method, COALESCE(card_last4, ''),
			       COALESCE(failure_code, ''), status, fee,
			       refunded_amount, disputed_amount, created_at
			FROM payments
			WHERE id = $1
		`, hold.PaymentID).Scan(
			&payment.ID,
			&payment.OrderID,
			&payment.Amount,
			&payment.Currency,
			&payment.Method,
			&payment.CardLast4,
			&payment.FailureCode,
			&payment.Status,
			&payment.Fee,
			&payment.RefundedAmount,
			&payment.DisputedAmount,
			&payment.CreatedAt,
		)
		if err != nil {
			return 0, err
		}

		var merchantID string

		err = tx.QueryRow(ctx, `
			SELECT merchant_id FROM orders WHERE id = $1
		`, hold.OrderID).Scan(&merchantID)
		if err != nil {
			return 0, err
		}

		if err := recordEventTx(
			ctx, tx, merchantID, EventPaymentVoided,
			payment.OrderID, payment.ID, "", "", payment,
		); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	return len(holds), nil
}

// purgeKeys deletes settled idempotency-key rows older than the TTL. Money
// rows (orders, payments, refunds, disputes, events) are NEVER garbage
// collected — the audit trail is forever; only the linkage rows go.
//
// Post-GC semantics are safe by construction: a retried key becomes a fresh
// claim (201 with a new record) rather than a false replay or a false 409.
// Clients must therefore treat keys as single-flight labels with a bounded
// lifetime, which the README states.
func (r *OrderRepository) purgeKeys(
	ctx context.Context,
	ttl time.Duration,
) (int, error) {
	var total int64

	for _, table := range []string{
		"idempotency_keys",
		"payment_idempotency_keys",
		"refund_idempotency_keys",
	} {
		tag, err := r.DB.Exec(ctx,
			`DELETE FROM `+table+` WHERE created_at < now() - make_interval(secs => $1)`,
			int(ttl.Seconds()),
		)
		if err != nil {
			return int(total), err
		}

		total += tag.RowsAffected()
	}

	return int(total), nil
}
