package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrOrderNotFound = errors.New("order not found")

var ErrOrderAlreadyPaid = errors.New("order is already paid")

var ErrPaymentAmountMismatch = errors.New(
	"payment amount must equal the order amount",
)

var ErrPaymentNotFound = errors.New("payment not found")

type Payment struct {
	ID             string    `json:"id"`
	OrderID        string    `json:"order_id"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	RefundedAmount int64     `json:"refunded_amount"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreatePaymentIdempotent records a mock-approved (SUCCEEDED) payment for a
// CREATED order and flips the order to SUCCEEDED, exactly once per
// (merchant, key):
//
//  1. Claim the key, then lock and inspect it. Same key + different request
//     hash is rejected with ErrIdempotencyConflict.
//  2. A linked payment_id is returned as-is (replay, created=false).
//  3. Otherwise the order row is locked and checked: it must exist, belong
//     to the merchant, still be CREATED, and match the requested amount.
//     Violations are ErrOrderNotFound, ErrOrderAlreadyPaid, and
//     ErrPaymentAmountMismatch — all returned before anything is written.
//  4. The payment is inserted, the order flipped, and the key linked — all
//     in one transaction, so a paid order always has its payment and vice
//     versa.
func (r *OrderRepository) CreatePaymentIdempotent(
	ctx context.Context,
	payment Payment,
	orderID string,
	merchantID string,
	key string,
	requestHash string,
) (Payment, bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Payment{}, false, err
	}
	defer tx.Rollback(ctx)

	// 1. Claim this merchant's payment key.
	_, err = tx.Exec(ctx, `
		INSERT INTO payment_idempotency_keys
			(merchant_id, key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (merchant_id, key) DO NOTHING
	`, merchantID, key, requestHash)
	if err != nil {
		return Payment{}, false, err
	}

	// 2. Lock and inspect the key's record.
	var storedHash string
	var existingPaymentID *string

	err = tx.QueryRow(ctx, `
		SELECT request_hash, payment_id::text
		FROM payment_idempotency_keys
		WHERE merchant_id = $1 AND key = $2
		FOR UPDATE
	`, merchantID, key).Scan(
		&storedHash,
		&existingPaymentID,
	)
	if err != nil {
		return Payment{}, false, err
	}

	// 3. Same key, different request: reject.
	if storedHash != requestHash {
		return Payment{}, false, ErrIdempotencyConflict
	}

	// 4. The payment already exists: return it.
	if existingPaymentID != nil {
		var existing Payment

		err = tx.QueryRow(ctx, `
			SELECT id::text, order_id::text, amount,
			       currency, status, refunded_amount, created_at
			FROM payments
			WHERE id = $1
		`, *existingPaymentID).Scan(
			&existing.ID,
			&existing.OrderID,
			&existing.Amount,
			&existing.Currency,
			&existing.Status,
			&existing.RefundedAmount,
			&existing.CreatedAt,
		)
		if err != nil {
			return Payment{}, false, err
		}

		if err := tx.Commit(ctx); err != nil {
			return Payment{}, false, err
		}

		return existing, false, nil
	}

	// 5. Lock the order and validate it is payable.
	var order Order

	err = tx.QueryRow(ctx, `
		SELECT id::text, merchant_id, amount,
		       currency, status, created_at
		FROM orders
		WHERE id = $1 AND merchant_id = $2
		FOR UPDATE
	`, orderID, merchantID).Scan(
		&order.ID,
		&order.MerchantID,
		&order.Amount,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, false, ErrOrderNotFound
	}

	if err != nil {
		return Payment{}, false, err
	}

	if order.Status != "CREATED" {
		return Payment{}, false, ErrOrderAlreadyPaid
	}

	if payment.Amount != order.Amount {
		return Payment{}, false, ErrPaymentAmountMismatch
	}

	// 6. Record the approved payment in the order's currency.
	payment.OrderID = order.ID
	payment.Currency = order.Currency
	payment.Status = "SUCCEEDED"

	err = tx.QueryRow(ctx, `
		INSERT INTO payments
			(id, order_id, amount, currency, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`,
		payment.ID,
		payment.OrderID,
		payment.Amount,
		payment.Currency,
		payment.Status,
	).Scan(&payment.CreatedAt)
	if err != nil {
		return Payment{}, false, err
	}

	// 7. Flip the order so it cannot be paid twice.
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		SET status = 'SUCCEEDED'
		WHERE id = $1
	`, order.ID); err != nil {
		return Payment{}, false, err
	}

	// 8. Associate the key with the payment.
	_, err = tx.Exec(ctx, `
		UPDATE payment_idempotency_keys
		SET payment_id = $1
		WHERE merchant_id = $2 AND key = $3
	`,
		payment.ID,
		merchantID,
		key,
	)
	if err != nil {
		return Payment{}, false, err
	}

	// 9. Commit payment, order flip, and key linkage together.
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, false, err
	}

	return payment, true, nil
}

// GetOrder returns one merchant-scoped order, or ErrOrderNotFound.
// Callers must shape-check id as UUID first: a malformed id would fail the
// Postgres UUID cast with an internal error instead of a 404.
func (r *OrderRepository) GetOrder(
	ctx context.Context,
	merchantID string,
	id string,
) (Order, error) {
	var order Order

	err := r.DB.QueryRow(ctx, `
		SELECT id::text, merchant_id, amount,
		       currency, status, created_at
		FROM orders
		WHERE id = $1 AND merchant_id = $2
	`, id, merchantID).Scan(
		&order.ID,
		&order.MerchantID,
		&order.Amount,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrOrderNotFound
	}

	if err != nil {
		return Order{}, err
	}

	return order, nil
}

// GetPayment returns one payment scoped to the merchant through its order,
// or ErrPaymentNotFound. The join prevents cross-merchant reads: payments
// carry no merchant_id of their own.
func (r *OrderRepository) GetPayment(
	ctx context.Context,
	merchantID string,
	id string,
) (Payment, error) {
	var payment Payment

	err := r.DB.QueryRow(ctx, `
		SELECT p.id::text, p.order_id::text, p.amount,
		       p.currency, p.status, p.refunded_amount, p.created_at
		FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE p.id = $1 AND o.merchant_id = $2
	`, id, merchantID).Scan(
		&payment.ID,
		&payment.OrderID,
		&payment.Amount,
		&payment.Currency,
		&payment.Status,
		&payment.RefundedAmount,
		&payment.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}

	if err != nil {
		return Payment{}, err
	}

	return payment, nil
}
