package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrDisputeNotFound = errors.New("dispute not found")

var ErrDisputeNotAllowed = errors.New(
	"payment cannot be disputed in its state",
)

var ErrDisputeNotOpen = errors.New("dispute is already resolved")

var ErrInvalidDispute = errors.New("invalid dispute reason or outcome")

// Dispute reasons are a closed allowlist: free text would fragment reporting
// ("fraud" vs "Fraud " vs "scam") and complicate downstream handling.
var DisputeReasons = map[string]bool{
	"fraud":        true,
	"not_received": true,
	"duplicate":    true,
	"cancelled":    true,
	"other":        true,
}

const (
	DisputeOpen = "OPEN"
	DisputeWon  = "WON"
	DisputeLost = "LOST"
)

const (
	OutcomeWon  = "won"
	OutcomeLost = "lost"
)

type Dispute struct {
	ID         string    `json:"id"`
	PaymentID  string    `json:"payment_id"`
	MerchantID string    `json:"merchant_id"`
	Amount     int64     `json:"amount"`
	Reason     string    `json:"reason"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// CurrencyNet is one row of the merchant balance: net receivable per
// currency. Balances never mix currencies — adding paise to cents is a
// category error, so the API returns a breakdown instead of a total.
type CurrencyNet struct {
	Currency string `json:"currency"`
	Net      int64  `json:"net_receivable"`
}

// OpenDispute withholds amount from a SUCCEEDED payment while the issuing
// bank investigates. The hold is a conditional increment of disputed_amount
// (same race-safe shape as the refund reservation), and replay-by-state
// replaces idempotency keys: an identical OPEN dispute for the payment is
// returned (created=false); a differing one conflicts. Only one OPEN
// dispute per payment can exist (partial unique index), so concurrent opens
// converge instead of double-holding.
func (r *OrderRepository) OpenDispute(
	ctx context.Context,
	merchantID string,
	paymentID string,
	disputeID string,
	amount int64,
	reason string,
) (Dispute, bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Dispute{}, false, err
	}
	defer tx.Rollback(ctx)

	var available int64

	err = tx.QueryRow(ctx, `
		SELECT p.amount - p.refunded_amount - p.disputed_amount
		FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE p.id = $1 AND o.merchant_id = $2 AND p.status = 'SUCCEEDED'
		FOR UPDATE OF p
	`, paymentID, merchantID).Scan(&available)

	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, false, ErrDisputeNotAllowed
	}

	if err != nil {
		return Dispute{}, false, err
	}

	if amount <= 0 || amount > available {
		return Dispute{}, false, ErrDisputeNotAllowed
	}

	var dispute Dispute

	err = tx.QueryRow(ctx, `
		INSERT INTO disputes
			(id, payment_id, merchant_id, amount, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (payment_id) WHERE status = 'OPEN' DO NOTHING
		RETURNING id::text, payment_id::text, merchant_id, amount,
		          reason, status, created_at
	`,
		disputeID,
		paymentID,
		merchantID,
		amount,
		reason,
	).Scan(
		&dispute.ID,
		&dispute.PaymentID,
		&dispute.MerchantID,
		&dispute.Amount,
		&dispute.Reason,
		&dispute.Status,
		&dispute.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		// An OPEN dispute already exists: identical terms replay it,
		// anything else conflicts with the outstanding hold.
		err = tx.QueryRow(ctx, `
			SELECT id::text, payment_id::text, merchant_id, amount,
			       reason, status, created_at
			FROM disputes
			WHERE payment_id = $1 AND status = 'OPEN'
		`, paymentID).Scan(
			&dispute.ID,
			&dispute.PaymentID,
			&dispute.MerchantID,
			&dispute.Amount,
			&dispute.Reason,
			&dispute.Status,
			&dispute.CreatedAt,
		)
		if err != nil {
			return Dispute{}, false, err
		}

		if dispute.Amount != amount || dispute.Reason != reason ||
			dispute.MerchantID != merchantID {
			return Dispute{}, false, ErrDisputeNotAllowed
		}

		if err := tx.Commit(ctx); err != nil {
			return Dispute{}, false, err
		}

		return dispute, false, nil
	}

	if err != nil {
		return Dispute{}, false, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE payments
		SET disputed_amount = disputed_amount + $1
		WHERE id = $2
	`, amount, paymentID); err != nil {
		return Dispute{}, false, err
	}

	if err := recordEventTx(
		ctx, tx, merchantID, EventDisputeOpened,
		"", paymentID, "", dispute.ID, dispute,
	); err != nil {
		return Dispute{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Dispute{}, false, err
	}

	return dispute, true, nil
}

// ResolveDispute closes an OPEN dispute. WON releases the hold back to the
// merchant's balance; LOST converts it into settled loss (refunded_amount
// rises, so the money is gone exactly as if refunded). Resolving twice
// replays; resolving anything non-OPEN is rejected.
func (r *OrderRepository) ResolveDispute(
	ctx context.Context,
	merchantID string,
	id string,
	outcome string,
) (Dispute, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Dispute{}, err
	}
	defer tx.Rollback(ctx)

	var dispute Dispute

	err = tx.QueryRow(ctx, `
		SELECT id::text, payment_id::text, merchant_id, amount,
		       reason, status, created_at
		FROM disputes
		WHERE id = $1 AND merchant_id = $2
		FOR UPDATE
	`, id, merchantID).Scan(
		&dispute.ID,
		&dispute.PaymentID,
		&dispute.MerchantID,
		&dispute.Amount,
		&dispute.Reason,
		&dispute.Status,
		&dispute.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrDisputeNotFound
	}

	if err != nil {
		return Dispute{}, err
	}

	if dispute.Status != DisputeOpen {
		return Dispute{}, ErrDisputeNotOpen
	}

	eventType := EventDisputeWon
	newStatus := DisputeWon

	_, err = tx.Exec(ctx, `
		UPDATE payments
		SET disputed_amount = disputed_amount - $1
		WHERE id = $2
	`, dispute.Amount, dispute.PaymentID)
	if err != nil {
		return Dispute{}, err
	}

	if outcome == OutcomeLost {
		newStatus = DisputeLost
		eventType = EventDisputeLost

		if _, err := tx.Exec(ctx, `
			UPDATE payments
			SET refunded_amount = refunded_amount + $1
			WHERE id = $2
		`, dispute.Amount, dispute.PaymentID); err != nil {
			return Dispute{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status = $2 WHERE id = $1
	`, dispute.ID, newStatus); err != nil {
		return Dispute{}, err
	}

	dispute.Status = newStatus

	if err := recordEventTx(
		ctx, tx, merchantID, eventType,
		"", dispute.PaymentID, "", dispute.ID, dispute,
	); err != nil {
		return Dispute{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Dispute{}, err
	}

	return dispute, nil
}

// GetDispute returns one merchant-scoped dispute, or ErrDisputeNotFound.
func (r *OrderRepository) GetDispute(
	ctx context.Context,
	merchantID string,
	id string,
) (Dispute, error) {
	var dispute Dispute

	err := r.DB.QueryRow(ctx, `
		SELECT id::text, payment_id::text, merchant_id, amount,
		       reason, status, created_at
		FROM disputes
		WHERE id = $1 AND merchant_id = $2
	`, id, merchantID).Scan(
		&dispute.ID,
		&dispute.PaymentID,
		&dispute.MerchantID,
		&dispute.Amount,
		&dispute.Reason,
		&dispute.Status,
		&dispute.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrDisputeNotFound
	}

	if err != nil {
		return Dispute{}, err
	}

	return dispute, nil
}

// ListDisputes returns the merchant's disputes, newest first, with the same
// keyset paging as every other list endpoint.
func (r *OrderRepository) ListDisputes(
	ctx context.Context,
	merchantID string,
	limit int,
	cursor string,
) ([]Dispute, bool, error) {
	const columns = `
		SELECT id::text, payment_id::text, merchant_id, amount,
		       reason, status, created_at
		FROM disputes
		WHERE merchant_id = $1
	`

	scan := func(rows pgx.Rows) ([]Dispute, error) {
		disputes := make([]Dispute, 0, limit+1)

		for rows.Next() {
			var dispute Dispute
			if err := rows.Scan(
				&dispute.ID,
				&dispute.PaymentID,
				&dispute.MerchantID,
				&dispute.Amount,
				&dispute.Reason,
				&dispute.Status,
				&dispute.CreatedAt,
			); err != nil {
				return nil, err
			}

			disputes = append(disputes, dispute)
		}

		return disputes, rows.Err()
	}

	if cursor == "" {
		rows, err := r.DB.Query(ctx, columns+`
			ORDER BY created_at DESC, id DESC
			LIMIT $2
		`, merchantID, limit+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()

		disputes, err := scan(rows)
		if err != nil {
			return nil, false, err
		}

		page, more := pageHasMore(disputes, limit)

		return page, more, nil
	}

	anchor, err := r.GetDispute(ctx, merchantID, cursor)
	if err != nil {
		return nil, false, ErrInvalidCursor
	}

	rows, err := r.DB.Query(ctx, columns+`
		  AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, merchantID, anchor.CreatedAt, anchor.ID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	disputes, err := scan(rows)
	if err != nil {
		return nil, false, err
	}

	page, more := pageHasMore(disputes, limit)

	return page, more, nil
}

// MerchantBalance reports net receivable per currency: captured, minus
// refunded, minus disputed holds, minus platform fees — all in minor units.
// Only SUCCEEDED payments count; holds and failures are not the merchant's
// money yet (or ever).
func (r *OrderRepository) MerchantBalance(
	ctx context.Context,
	merchantID string,
) ([]CurrencyNet, error) {
	nets := make([]CurrencyNet, 0)

	rows, err := r.DB.Query(ctx, `
		SELECT o.currency,
		       COALESCE(SUM(p.amount - p.refunded_amount - p.disputed_amount - p.fee), 0)
		FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE o.merchant_id = $1 AND p.status = 'SUCCEEDED'
		GROUP BY o.currency
		ORDER BY o.currency
	`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var net CurrencyNet
		if err := rows.Scan(&net.Currency, &net.Net); err != nil {
			return nil, err
		}

		nets = append(nets, net)
	}

	return nets, rows.Err()
}
