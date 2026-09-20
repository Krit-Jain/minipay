package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrIdempotencyConflict = errors.New(
	"idempotency key reused with different request",
)

type Order struct {
	ID         string    `json:"id"`
	MerchantID string    `json:"merchant_id"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

type OrderRepository struct {
	DB *pgxpool.Pool
}

func (r *OrderRepository) CreateIdempotent(
	ctx context.Context,
	order Order,
	key string,
	requestHash string,
) (Order, bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Order{}, false, err
	}
	defer tx.Rollback(ctx)

	// 1. Claim this merchant's idempotency key.
	_, err = tx.Exec(ctx, `
		INSERT INTO idempotency_keys
			(merchant_id, key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (merchant_id, key) DO NOTHING
	`, order.MerchantID, key, requestHash)
	if err != nil {
		return Order{}, false, err
	}

	// 2. Lock and inspect the key's record.
	var storedHash string
	var existingOrderID *string

	err = tx.QueryRow(ctx, `
		SELECT request_hash, order_id::text
		FROM idempotency_keys
		WHERE merchant_id = $1 AND key = $2
		FOR UPDATE
	`, order.MerchantID, key).Scan(
		&storedHash,
		&existingOrderID,
	)
	if err != nil {
		return Order{}, false, err
	}

	// 3. Same key, different request: reject.
	if storedHash != requestHash {
		return Order{}, false, ErrIdempotencyConflict
	}

	// 4. The order already exists: return it.
	if existingOrderID != nil {
		var existing Order

		err = tx.QueryRow(ctx, `
			SELECT id::text, merchant_id, amount,
			       currency, status, created_at
			FROM orders
			WHERE id = $1
		`, *existingOrderID).Scan(
			&existing.ID,
			&existing.MerchantID,
			&existing.Amount,
			&existing.Currency,
			&existing.Status,
			&existing.CreatedAt,
		)
		if err != nil {
			return Order{}, false, err
		}

		if err := tx.Commit(ctx); err != nil {
			return Order{}, false, err
		}

		return existing, false, nil
	}

	// 5. No order is associated with this key yet.
	// Create it within this same transaction.
	err = tx.QueryRow(ctx, `
		INSERT INTO orders
			(id, merchant_id, amount, currency, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`,
		order.ID,
		order.MerchantID,
		order.Amount,
		order.Currency,
		order.Status,
	).Scan(&order.CreatedAt)

	if err != nil {
		return Order{}, false, err
	}

	// 6. Associate the key with the new order.
	_, err = tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET order_id = $1
		WHERE merchant_id = $2 AND key = $3
	`,
		order.ID,
		order.MerchantID,
		key,
	)
	if err != nil {
		return Order{}, false, err
	}

	// 7. Commit both records together.
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, err
	}

	return order, true, nil
}

// Keep this method if other code still uses the old Create API.
func (r *OrderRepository) Create(
	ctx context.Context,
	order Order,
) error {
	_, err := r.DB.Exec(
		ctx,
		`INSERT INTO orders
			(id, merchant_id, amount, currency, status)
		 VALUES ($1, $2, $3, $4, $5)`,
		order.ID,
		order.MerchantID,
		order.Amount,
		order.Currency,
		order.Status,
	)

	return err
}

// Keep this import useful for compile-time reference to pgx.
var _ = pgx.ErrNoRows
