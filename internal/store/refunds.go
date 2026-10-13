package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRefundNotAllowed = errors.New(
	"payment not found, not successful, or insufficient refundable balance",
)

type Refund struct {
	ID        string    `json:"id"`
	PaymentID string    `json:"payment_id"`
	Amount    int64     `json:"amount"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *OrderRepository) CreateRefund(
	ctx context.Context,
	paymentID string,
	refundID string,
	amount int64,
) (Refund, error) {	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Refund{}, err
	}
	defer tx.Rollback(ctx)

	// 1. Atomically reserve the refundable balance.
	var updatedPaymentID string

	err = tx.QueryRow(ctx, `
		UPDATE payments
		SET refunded_amount = refunded_amount + $1
		WHERE id = $2
		  AND status = 'SUCCEEDED'
		  AND $1 > 0
		  AND $1 <= amount
		  AND refunded_amount <= amount - $1
		RETURNING id::text
	`,
		amount,
		paymentID,
	).Scan(&updatedPaymentID)

	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, ErrRefundNotAllowed
	}

	if err != nil {
		return Refund{}, err
	}

	// 2. Insert the individual refund record.
	var refund Refund

	err = tx.QueryRow(ctx, `
		INSERT INTO refunds (
			id,
			payment_id,
			amount,
			status
		)
		VALUES ($1, $2, $3, 'PENDING')
		RETURNING
			id::text,
			payment_id::text,
			amount,
			status,
			created_at
	`,
		refundID,
		paymentID,
		amount,
	).Scan(
		&refund.ID,
		&refund.PaymentID,
		&refund.Amount,
		&refund.Status,
		&refund.CreatedAt,
	)

	if err != nil {
		return Refund{}, err
	}

	// 3. Commit the reservation and refund record together.
	if err := tx.Commit(ctx); err != nil {
		return Refund{}, err
	}

	return refund, nil
}

// CreateRefundIdempotent reserves balance and records the refund exactly
// once per (merchant, key), mirroring the order-side protocol:
//
//  1. Claim the key, then lock and inspect it. Same key + different
//     request hash is rejected with ErrIdempotencyConflict.
//  2. A linked refund_id is returned as-is (replay, created=false).
//  3. Otherwise the refund row is inserted FIRST with
//     balance_reserved=false, guarded by the UNIQUE index on
//     (payment_id, idempotency_key), so a replay after a crash finds the
//     row instead of recording a second refund.
//  4. The balance reservation (the same conditional UPDATE as
//     CreateRefund) runs only when the row is not yet reserved, then the
//     row is flagged and the key linked — all in one transaction.
//     A reservation failure rolls everything back, leaving no orphan
//     PENDING row behind.
func (r *OrderRepository) CreateRefundIdempotent(
	ctx context.Context,
	paymentID string,
	refundID string,
	amount int64,
	merchantID string,
	key string,
	requestHash string,
) (Refund, bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Refund{}, false, err
	}
	defer tx.Rollback(ctx)

	// 1. Claim this merchant's refund key.
	_, err = tx.Exec(ctx, `
		INSERT INTO refund_idempotency_keys
			(merchant_id, key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (merchant_id, key) DO NOTHING
	`, merchantID, key, requestHash)
	if err != nil {
		return Refund{}, false, err
	}

	// 2. Lock and inspect the key's record.
	var storedHash string
	var existingRefundID *string

	err = tx.QueryRow(ctx, `
		SELECT request_hash, refund_id::text
		FROM refund_idempotency_keys
		WHERE merchant_id = $1 AND key = $2
		FOR UPDATE
	`, merchantID, key).Scan(
		&storedHash,
		&existingRefundID,
	)
	if err != nil {
		return Refund{}, false, err
	}

	// 3. Same key, different request: reject.
	if storedHash != requestHash {
		return Refund{}, false, ErrIdempotencyConflict
	}

	// 4. The refund already exists: return it.
	if existingRefundID != nil {
		var existing Refund

		err = tx.QueryRow(ctx, `
			SELECT id::text, payment_id::text, amount, status, created_at
			FROM refunds
			WHERE id = $1
		`, *existingRefundID).Scan(
			&existing.ID,
			&existing.PaymentID,
			&existing.Amount,
			&existing.Status,
			&existing.CreatedAt,
		)
		if err != nil {
			return Refund{}, false, err
		}

		if err := tx.Commit(ctx); err != nil {
			return Refund{}, false, err
		}

		return existing, false, nil
	}

	// 5. Insert the refund record first, unreserved. ON CONFLICT means a
	// previous attempt crashed after inserting but before linking the key.
	var refund Refund
	var reserved bool

	ours := true

	err = tx.QueryRow(ctx, `
		INSERT INTO refunds (
			id,
			payment_id,
			amount,
			status,
			idempotency_key,
			balance_reserved
		)
		VALUES ($1, $2, $3, 'PENDING', $4, FALSE)
		ON CONFLICT (payment_id, idempotency_key) DO NOTHING
		RETURNING
			id::text,
			payment_id::text,
			amount,
			status,
			created_at,
			balance_reserved
	`,
		refundID,
		paymentID,
		amount,
		key,
	).Scan(
		&refund.ID,
		&refund.PaymentID,
		&refund.Amount,
		&refund.Status,
		&refund.CreatedAt,
		&reserved,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		ours = false

		err = tx.QueryRow(ctx, `
			SELECT id::text, payment_id::text, amount,
			       status, created_at, balance_reserved
			FROM refunds
			WHERE payment_id = $1 AND idempotency_key = $2
			FOR UPDATE
		`, paymentID, key).Scan(
			&refund.ID,
			&refund.PaymentID,
			&refund.Amount,
			&refund.Status,
			&refund.CreatedAt,
			&reserved,
		)
	}

	if err != nil {
		return Refund{}, false, err
	}

	// 6. Reserve the refundable balance unless a previous attempt already
	// did. A failure here rolls back the whole transaction, including the
	// PENDING row inserted above.
	if !reserved {
		var updatedPaymentID string

		err = tx.QueryRow(ctx, `
			UPDATE payments
			SET refunded_amount = refunded_amount + $1
			WHERE id = $2
			  AND status = 'SUCCEEDED'
			  AND $1 > 0
			  AND $1 <= amount
			  AND refunded_amount <= amount - $1
			RETURNING id::text
		`,
			amount,
			paymentID,
		).Scan(&updatedPaymentID)

		if errors.Is(err, pgx.ErrNoRows) {
			return Refund{}, false, ErrRefundNotAllowed
		}

		if err != nil {
			return Refund{}, false, err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE refunds
			SET balance_reserved = TRUE
			WHERE id = $1
		`, refund.ID); err != nil {
			return Refund{}, false, err
		}
	}

	// 7. Associate the key with the refund.
	_, err = tx.Exec(ctx, `
		UPDATE refund_idempotency_keys
		SET refund_id = $1
		WHERE merchant_id = $2 AND key = $3
	`,
		refund.ID,
		merchantID,
		key,
	)
	if err != nil {
		return Refund{}, false, err
	}

	// 8. Commit the key, the reservation, and the record together.
	if err := tx.Commit(ctx); err != nil {
		return Refund{}, false, err
	}

	return refund, ours, nil
}
