package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidCursor = errors.New("invalid starting_after cursor")

// ListResult is the page both list endpoints share: up to limit rows,
// newest first, and whether another page follows. Clients walk pages by
// passing the last row's id as ?starting_after=.
//
// Pages use keyset (a.k.a. cursor) pagination — "rows older than this
// timestamp+id" — instead of OFFSET, because OFFSET counts from the start
// every time: rows inserted between page fetches shift every later page,
// silently skipping or duplicating records. A cursor pins the position.
func pageHasMore[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) <= limit {
		return rows, false
	}

	return rows[:limit], true
}

// resolveOrderCursor maps a ?starting_after= id to its timestamp for the
// tuple comparison. Unknown or foreign ids are a client error, not an
// empty page: paginating from nothing would silently restart or mislead.
func (r *OrderRepository) resolveOrderCursor(
	ctx context.Context,
	merchantID string,
	cursor string,
) (Order, error) {
	order, err := r.GetOrder(ctx, merchantID, cursor)
	if err != nil {
		return Order{}, ErrInvalidCursor
	}

	return order, nil
}

// ListOrders returns the merchant's orders, newest first.
func (r *OrderRepository) ListOrders(
	ctx context.Context,
	merchantID string,
	limit int,
	cursor string,
) ([]Order, bool, error) {
	orders := make([]Order, 0, limit+1)

	if cursor == "" {
		rows, err := r.DB.Query(ctx, `
			SELECT id::text, merchant_id, amount,
			       currency, status, created_at
			FROM orders
			WHERE merchant_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2
		`, merchantID, limit+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()

		for rows.Next() {
			var order Order
			if err := rows.Scan(
				&order.ID,
				&order.MerchantID,
				&order.Amount,
				&order.Currency,
				&order.Status,
				&order.CreatedAt,
			); err != nil {
				return nil, false, err
			}

			orders = append(orders, order)
		}

		if err := rows.Err(); err != nil {
			return nil, false, err
		}

		page, more := pageHasMore(orders, limit)

		return page, more, nil
	}

	anchor, err := r.resolveOrderCursor(ctx, merchantID, cursor)
	if err != nil {
		return nil, false, err
	}

	rows, err := r.DB.Query(ctx, `
		SELECT id::text, merchant_id, amount,
		       currency, status, created_at
		FROM orders
		WHERE merchant_id = $1
		  AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, merchantID, anchor.CreatedAt, anchor.ID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var order Order
		if err := rows.Scan(
			&order.ID,
			&order.MerchantID,
			&order.Amount,
			&order.Currency,
			&order.Status,
			&order.CreatedAt,
		); err != nil {
			return nil, false, err
		}

		orders = append(orders, order)
	}

	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	page, more := pageHasMore(orders, limit)

	return page, more, nil
}

// ListPayments returns the merchant's payments (scoped through their
// orders), newest first.
func (r *OrderRepository) ListPayments(
	ctx context.Context,
	merchantID string,
	limit int,
	cursor string,
) ([]Payment, bool, error) {
	const columns = `
		SELECT p.id::text, p.order_id::text, p.amount,
		       p.currency, p.method, COALESCE(p.card_last4, ''),
		       COALESCE(p.failure_code, ''), p.status,
		       p.refunded_amount, p.created_at
		FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE o.merchant_id = $1
	`

	scan := func(rows pgx.Rows) ([]Payment, error) {
		payments := make([]Payment, 0, limit+1)

		for rows.Next() {
			var payment Payment
			if err := rows.Scan(
				&payment.ID,
				&payment.OrderID,
				&payment.Amount,
				&payment.Currency,
				&payment.Method,
				&payment.CardLast4,
				&payment.FailureCode,
				&payment.Status,
				&payment.RefundedAmount,
				&payment.CreatedAt,
			); err != nil {
				return nil, err
			}

			payments = append(payments, payment)
		}

		return payments, rows.Err()
	}

	if cursor == "" {
		rows, err := r.DB.Query(ctx, columns+`
			ORDER BY p.created_at DESC, p.id DESC
			LIMIT $2
		`, merchantID, limit+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()

		payments, err := scan(rows)
		if err != nil {
			return nil, false, err
		}

		page, more := pageHasMore(payments, limit)

		return page, more, nil
	}

	anchor, err := r.GetPayment(ctx, merchantID, cursor)
	if err != nil {
		return nil, false, ErrInvalidCursor
	}

	rows, err := r.DB.Query(ctx, columns+`
		  AND (p.created_at, p.id) < ($2, $3::uuid)
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $4
	`, merchantID, anchor.CreatedAt, anchor.ID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	payments, err := scan(rows)
	if err != nil {
		return nil, false, err
	}

	page, more := pageHasMore(payments, limit)

	return page, more, nil
}

// ListRefunds returns one payment's refunds, newest first. A payment that
// is missing or belongs to another merchant is ErrPaymentNotFound —
// consistent with GetPayment, and it leaks nothing across tenants.
func (r *OrderRepository) ListRefunds(
	ctx context.Context,
	merchantID string,
	paymentID string,
	limit int,
	cursor string,
) ([]Refund, bool, error) {
	if _, err := r.GetPayment(ctx, merchantID, paymentID); err != nil {
		return nil, false, err
	}

	const columns = `
		SELECT id::text, payment_id::text, amount, status, created_at
		FROM refunds
		WHERE payment_id = $1
	`

	scan := func(rows pgx.Rows) ([]Refund, error) {
		refunds := make([]Refund, 0, limit+1)

		for rows.Next() {
			var refund Refund
			if err := rows.Scan(
				&refund.ID,
				&refund.PaymentID,
				&refund.Amount,
				&refund.Status,
				&refund.CreatedAt,
			); err != nil {
				return nil, err
			}

			refunds = append(refunds, refund)
		}

		return refunds, rows.Err()
	}

	if cursor == "" {
		rows, err := r.DB.Query(ctx, columns+`
			ORDER BY created_at DESC, id DESC
			LIMIT $2
		`, paymentID, limit+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()

		refunds, err := scan(rows)
		if err != nil {
			return nil, false, err
		}

		page, more := pageHasMore(refunds, limit)

		return page, more, nil
	}

	var anchor Refund

	err := r.DB.QueryRow(ctx, `
		SELECT id::text, payment_id::text, amount, status, created_at
		FROM refunds
		WHERE id = $1 AND payment_id = $2
	`, cursor, paymentID).Scan(
		&anchor.ID,
		&anchor.PaymentID,
		&anchor.Amount,
		&anchor.Status,
		&anchor.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrInvalidCursor
	}

	if err != nil {
		return nil, false, err
	}

	rows, err := r.DB.Query(ctx, columns+`
		  AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, paymentID, anchor.CreatedAt, anchor.ID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	refunds, err := scan(rows)
	if err != nil {
		return nil, false, err
	}

	page, more := pageHasMore(refunds, limit)

	return page, more, nil
}
