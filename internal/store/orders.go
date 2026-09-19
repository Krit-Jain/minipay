package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Order struct {
	ID         string
	MerchantID string
	Amount     int64
	Currency   string
	Status     string
}

type OrderRepository struct {
	DB *pgxpool.Pool
}

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
