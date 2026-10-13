package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"minipay/internal/webhooks"
)

// Event types. The set grows by adding a constant plus one recordEventTx
// call at the state change; receivers switch on type, never on shape.
const (
	EventOrderCreated      = "order.created"
	EventPaymentAuthorized = "payment.authorized"
	EventPaymentFailed     = "payment.failed"
	EventPaymentCaptured   = "payment.captured"
	EventPaymentVoided     = "payment.voided"
	EventRefundCreated     = "refund.created"
)

var ErrEndpointNotFound = errors.New("webhook endpoint not found")

var ErrInvalidEndpointURL = errors.New("endpoint URL must be http(s)")

type Endpoint struct {
	ID         string    `json:"id"`
	MerchantID string    `json:"merchant_id,omitempty"`
	URL        string    `json:"url"`
	Secret     string    `json:"secret,omitempty"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

// Relay tuning: 20 deliveries per 2s tick, 25 attempts max (2s, 4s, 8s, ...
// capped at 5min — roughly a day of trying before dead-lettering).
const (
	RelayBatchSize   = 20
	RelayMaxAttempts = 25
	relayLease       = 30 * time.Second
	relayMaxBackoff  = 5 * time.Minute
)

func relayBackoff(attempts int) time.Duration {
	shift := attempts
	if shift > 8 {
		shift = 8
	}

	backoff := time.Duration(1<<shift) * time.Second
	if backoff > relayMaxBackoff {
		backoff = relayMaxBackoff
	}

	return backoff
}

// CreateEndpoint registers a delivery URL and mints its signing secret. The
// secret is returned once — ListEndpoints never exposes it — mirroring API
// key issuance. Unlike API keys it is stored retrievable: the relay needs
// the raw secret to sign every delivery (see migration 0009).
func (r *OrderRepository) CreateEndpoint(
	ctx context.Context,
	merchantID string,
	url string,
) (Endpoint, error) {
	secret, err := newSecret()
	if err != nil {
		return Endpoint{}, err
	}

	id, err := newUUID()
	if err != nil {
		return Endpoint{}, err
	}

	var endpoint Endpoint

	err = r.DB.QueryRow(ctx, `
		INSERT INTO merchant_endpoints (id, merchant_id, url, secret)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, merchant_id, url, secret, active, created_at
	`, id, merchantID, url, secret).Scan(
		&endpoint.ID,
		&endpoint.MerchantID,
		&endpoint.URL,
		&endpoint.Secret,
		&endpoint.Active,
		&endpoint.CreatedAt,
	)
	if err != nil {
		return Endpoint{}, err
	}

	return endpoint, nil
}

// ListEndpoints returns the merchant's endpoints without secrets.
func (r *OrderRepository) ListEndpoints(
	ctx context.Context,
	merchantID string,
) ([]Endpoint, error) {
	endpoints := make([]Endpoint, 0)

	rows, err := r.DB.Query(ctx, `
		SELECT id::text, merchant_id, url, active, created_at
		FROM merchant_endpoints
		WHERE merchant_id = $1
		ORDER BY created_at DESC
	`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var endpoint Endpoint
		if err := rows.Scan(
			&endpoint.ID,
			&endpoint.MerchantID,
			&endpoint.URL,
			&endpoint.Active,
			&endpoint.CreatedAt,
		); err != nil {
			return nil, err
		}

		endpoints = append(endpoints, endpoint)
	}

	return endpoints, rows.Err()
}

// DeactivateEndpoint stops future fan-out to the URL. In-flight pending
// deliveries are failed by the relay (they can never be delivered), while
// past SUCCEEDED rows stay as the audit trail.
func (r *OrderRepository) DeactivateEndpoint(
	ctx context.Context,
	merchantID string,
	id string,
) error {
	tag, err := r.DB.Exec(ctx, `
		UPDATE merchant_endpoints
		SET active = FALSE
		WHERE id = $1 AND merchant_id = $2 AND active
	`, id, merchantID)
	if err != nil {
		return err
	}

	if tag.RowsAffected() == 0 {
		return ErrEndpointNotFound
	}

	return nil
}

// recordEventTx writes the event and fans out one PENDING delivery per
// active endpoint — inside the caller's transaction, so the state change
// and its notification commit atomically. Call it only on first completion
// (never on idempotent replay): replay means the event already exists.
//
// Atomicity is the whole point of the outbox: "payment captured but no
// event" and "event for a payment that rolled back" are both impossible,
// which polling ("did anything change?") and after-commit hooks ("tell them
// if we get that far") can never guarantee under crashes.
func recordEventTx(
	ctx context.Context,
	tx pgx.Tx,
	merchantID string,
	eventType string,
	orderID string,
	paymentID string,
	refundID string,
	payload any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	eventID, err := newUUID()
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO events
			(id, merchant_id, type, order_id, payment_id, refund_id, payload)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid,
		        NULLIF($6, '')::uuid, $7::jsonb)
	`, eventID, merchantID, eventType, orderID, paymentID, refundID, string(body))
	if err != nil {
		return err
	}

	// Fan out after the SELECT is fully consumed: Postgres/pgx issues
	// queries on one connection per transaction, so interleaving the
	// endpoint scan with INSERTs on the same tx fails with "conn busy".
	var endpointIDs []string

	rows, err := tx.Query(ctx, `
		SELECT id::text
		FROM merchant_endpoints
		WHERE merchant_id = $1 AND active
	`, merchantID)
	if err != nil {
		return err
	}

	for rows.Next() {
		var endpointID string
		if err := rows.Scan(&endpointID); err != nil {
			rows.Close()
			return err
		}

		endpointIDs = append(endpointIDs, endpointID)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}

	rows.Close()

	for _, endpointID := range endpointIDs {
		deliveryID, err := newUUID()
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries (id, event_id, endpoint_id)
			VALUES ($1, $2, $3)
		`, deliveryID, eventID, endpointID); err != nil {
			return err
		}
	}

	return nil
}

// delivery is one claimed unit of relay work.
type delivery struct {
	ID         string
	EventID    string
	EndpointID string
	Attempts   int
	EventType  string
	Payload    []byte
	URL        string
	Secret     string
	Active     bool
}

// ProcessDueDeliveries claims due deliveries (SKIP LOCKED, so N relay
// replicas share work instead of duplicating it), POSTs each outside any
// transaction, then records the outcome. Claiming bumps attempts and parks
// next_attempt_at on a 30s lease: a crashed relay's claims expire back into
// the queue instead of sticking forever — at-least-once delivery, so
// receivers must dedupe on event id. Returns settled count.
func (r *OrderRepository) ProcessDueDeliveries(
	ctx context.Context,
	client *http.Client,
) (int, error) {
	claimed, err := r.claimDue(ctx)
	if err != nil {
		return 0, err
	}

	settled := 0

	for _, d := range claimed {
		if !d.Active {
			if err := r.failDelivery(ctx, d.ID); err != nil {
				return settled, err
			}

			settled++

			continue
		}

		if err := postDelivery(ctx, client, d); err != nil {
			if err := r.retryDelivery(ctx, d); err != nil {
				return settled, err
			}

			continue
		}

		if err := r.succeedDelivery(ctx, d.ID); err != nil {
			return settled, err
		}

		settled++
	}

	return settled, nil
}

func (r *OrderRepository) claimDue(ctx context.Context) ([]delivery, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT d.id::text, d.event_id::text, d.endpoint_id::text, d.attempts,
		       e.type, e.payload::text, ep.url, ep.secret, ep.active
		FROM deliveries d
		JOIN events e ON e.id = d.event_id
		JOIN merchant_endpoints ep ON ep.id = d.endpoint_id
		WHERE d.status = 'PENDING' AND d.next_attempt_at <= now()
		ORDER BY d.next_attempt_at
		LIMIT $1
		FOR UPDATE OF d SKIP LOCKED
	`, RelayBatchSize)
	if err != nil {
		return nil, err
	}

	var claimed []delivery

	for rows.Next() {
		var (
			d        delivery
			payload  string
			attempts int
			active   bool
		)

		if err := rows.Scan(
			&d.ID,
			&d.EventID,
			&d.EndpointID,
			&attempts,
			&d.EventType,
			&payload,
			&d.URL,
			&d.Secret,
			&active,
		); err != nil {
			rows.Close()
			return nil, err
		}

		d.Attempts = attempts + 1
		d.Payload = []byte(payload)
		d.Active = active
		claimed = append(claimed, d)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}

	rows.Close()

	// Leases are written after the SELECT is drained: same conn-busy rule
	// as the fan-out (one connection per transaction).
	for _, d := range claimed {
		if _, err := tx.Exec(ctx, `
			UPDATE deliveries
			SET attempts = $2, next_attempt_at = now() + make_interval(secs => $3)
			WHERE id = $1
		`, d.ID, d.Attempts, int(relayLease.Seconds())); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return claimed, nil
}

func postDelivery(
	ctx context.Context,
	client *http.Client,
	d delivery,
) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		d.URL,
		bytes.NewReader(d.Payload),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Minipay-Event", d.EventType)
	req.Header.Set("X-Minipay-Event-ID", d.EventID)
	req.Header.Set("X-Minipay-Attempt", fmt.Sprint(d.Attempts))
	req.Header.Set("X-Minipay-Signature", webhooks.Sign(d.Secret, d.Payload))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}

	return nil
}

func (r *OrderRepository) succeedDelivery(ctx context.Context, id string) error {
	_, err := r.DB.Exec(ctx, `
		UPDATE deliveries SET status = 'SUCCEEDED' WHERE id = $1
	`, id)

	return err
}

func (r *OrderRepository) failDelivery(ctx context.Context, id string) error {
	_, err := r.DB.Exec(ctx, `
		UPDATE deliveries SET status = 'FAILED' WHERE id = $1
	`, id)

	return err
}

func (r *OrderRepository) retryDelivery(ctx context.Context, d delivery) error {
	if d.Attempts >= RelayMaxAttempts {
		return r.failDelivery(ctx, d.ID)
	}

	_, err := r.DB.Exec(ctx, `
		UPDATE deliveries
		SET next_attempt_at = now() + make_interval(secs => $2)
		WHERE id = $1
	`, d.ID, int(relayBackoff(d.Attempts).Seconds()))

	return err
}

// newSecret mints a webhook signing secret. Stored retrievable (see
// migration 0009): unlike API keys, the relay needs the raw value.
func newSecret() (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", err
	}

	return "whsec_" + id[:8] + id[9:13] + id[14:18], nil
}
