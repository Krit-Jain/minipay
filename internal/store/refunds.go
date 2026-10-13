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
) (Refund, error) {
	tx, err := r.DB.Begin(ctx)
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
