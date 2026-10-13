package store

// DB integration tests for the webhook outbox and relay. They run against
// DATABASE_URL and skip otherwise.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"minipay/internal/webhooks"
)

func TestEndpointCRUD(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	endpoint, err := repo.CreateEndpoint(ctx, merchant, "https://example.com/hook")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	if endpoint.Secret == "" || !endpoint.Active {
		t.Fatalf("endpoint missing secret or inactive: %+v", endpoint)
	}

	listed, err := repo.ListEndpoints(ctx, merchant)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}

	if len(listed) != 1 || listed[0].ID != endpoint.ID {
		t.Fatalf("list: unexpected %+v", listed)
	}

	if listed[0].Secret != "" {
		t.Fatal("list leaks the signing secret")
	}

	if err := repo.DeactivateEndpoint(ctx, merchant, endpoint.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	if err := repo.DeactivateEndpoint(ctx, merchant, endpoint.ID); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("double deactivate: got %v, want ErrEndpointNotFound", err)
	}

	if err := repo.DeactivateEndpoint(ctx, merchant, testUUID(t)); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("unknown endpoint: got %v, want ErrEndpointNotFound", err)
	}
}

// cleanupMerchant removes every row a test created for the merchant, in
// FK-safe order, so suites leave the database as they found it.
func cleanupMerchant(t *testing.T, repo *OrderRepository, merchant string) {
	t.Helper()

	t.Cleanup(func() {
		ctx := context.Background()
		stmts := []string{
			`DELETE FROM deliveries WHERE event_id IN (SELECT id FROM events WHERE merchant_id = $1)`,
			`DELETE FROM events WHERE merchant_id = $1`,
			`DELETE FROM merchant_endpoints WHERE merchant_id = $1`,
			`DELETE FROM disputes WHERE merchant_id = $1`,
			`DELETE FROM refund_idempotency_keys WHERE merchant_id = $1`,
			`DELETE FROM payment_idempotency_keys WHERE merchant_id = $1`,
			`DELETE FROM idempotency_keys WHERE merchant_id = $1`,
			`DELETE FROM refunds WHERE payment_id IN (SELECT p.id FROM payments p JOIN orders o ON o.id = p.order_id WHERE o.merchant_id = $1)`,
			`DELETE FROM payments WHERE order_id IN (SELECT id FROM orders WHERE merchant_id = $1)`,
			`DELETE FROM orders WHERE merchant_id = $1`,
		}

		for _, stmt := range stmts {
			if _, err := repo.DB.Exec(ctx, stmt, merchant); err != nil {
				t.Errorf("cleanup %q: %v", merchant, err)
			}
		}
	})
}

func merchantEventTypes(t *testing.T, repo *OrderRepository, merchant string) []string {
	t.Helper()

	rows, err := repo.DB.Query(context.Background(), `
		SELECT type FROM events WHERE merchant_id = $1 ORDER BY created_at
	`, merchant)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	var types []string

	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan event: %v", err)
		}

		types = append(types, typ)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("events rows: %v", err)
	}

	return types
}

func merchantPendingDeliveries(t *testing.T, repo *OrderRepository, merchant string) int {
	t.Helper()

	var n int

	err := repo.DB.QueryRow(context.Background(), `
		SELECT count(*)
		FROM deliveries d
		JOIN events e ON e.id = d.event_id
		WHERE e.merchant_id = $1 AND d.status = 'PENDING'
	`, merchant).Scan(&n)
	if err != nil {
		t.Fatalf("count deliveries: %v", err)
	}

	return n
}

func TestWebhookFanoutAndTypes(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	// Two endpoints: every event fans out twice. The URLs are never called
	// in this test — only the outbox rows are asserted.
	for _, url := range []string{"http://localhost:9/hook-a", "http://localhost:9/hook-b"} {
		if _, err := repo.CreateEndpoint(ctx, merchant, url); err != nil {
			t.Fatalf("create endpoint: %v", err)
		}
	}

	order, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "wh-order-key", "whash-order")
	if err != nil {
		t.Fatalf("order: %v", err)
	}

	payment, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 50000, Method: "card", Status: "AUTHORIZED"},
		order.ID,
		merchant,
		"wh-pay-key",
		"whash-pay",
	)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	if _, err := repo.CapturePayment(ctx, merchant, payment.ID, 1300); err != nil {
		t.Fatalf("capture: %v", err)
	}

	if _, _, err := repo.CreateRefundIdempotent(
		ctx, payment.ID, testUUID(t), 10000, merchant, "wh-ref-key", "whash-ref",
	); err != nil {
		t.Fatalf("refund: %v", err)
	}

	// A voided authorization and a declined payment complete the type set.
	order2, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     10000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "wh-order2-key", "whash-order2")
	if err != nil {
		t.Fatalf("order2: %v", err)
	}

	held, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{ID: testUUID(t), Amount: 10000, Method: "card", Status: "AUTHORIZED"},
		order2.ID,
		merchant,
		"wh-pay2-key",
		"whash-pay2",
	)
	if err != nil {
		t.Fatalf("authorize2: %v", err)
	}

	if _, err := repo.VoidPayment(ctx, merchant, held.ID); err != nil {
		t.Fatalf("void: %v", err)
	}

	order3, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     20000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "wh-order3-key", "whash-order3")
	if err != nil {
		t.Fatalf("order3: %v", err)
	}

	if _, _, err := repo.CreatePaymentIdempotent(
		ctx,
		Payment{
			ID:          testUUID(t),
			Amount:      20000,
			Method:      "card",
			CardLast4:   "0002",
			FailureCode: "card_declined",
			Status:      "FAILED",
		},
		order3.ID,
		merchant,
		"wh-pay3-key",
		"whash-pay3",
	); err != nil {
		t.Fatalf("decline: %v", err)
	}

	want := []string{
		EventOrderCreated,
		EventPaymentAuthorized,
		EventPaymentCaptured,
		EventRefundCreated,
		EventOrderCreated,
		EventPaymentAuthorized,
		EventPaymentVoided,
		EventOrderCreated,
		EventPaymentFailed,
	}

	got := merchantEventTypes(t, repo, merchant)

	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}

	// 9 events x 2 endpoints, all pending.
	if n := merchantPendingDeliveries(t, repo, merchant); n != 18 {
		t.Fatalf("pending deliveries = %d, want 18", n)
	}

	// Replays are silent: the same order key must not emit again.
	if _, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "wh-order-key", "whash-order"); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := merchantEventTypes(t, repo, merchant); len(got) != len(want) {
		t.Fatalf("after replay: %d events, want %d (no duplicates)", len(got), len(want))
	}
}

type hookRecorder struct {
	mu      sync.Mutex
	bodies  [][]byte
	signals []string
	ids     []string
	status  []int
}

func (h *hookRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()

		code := 200
		if len(h.bodies) < len(h.status) {
			code = h.status[len(h.bodies)]
		}

		body, _ := io.ReadAll(r.Body)

		h.bodies = append(h.bodies, body)
		h.signals = append(h.signals, r.Header.Get("X-Minipay-Signature"))
		h.ids = append(h.ids, r.Header.Get("X-Minipay-Event-ID"))
		w.WriteHeader(code)
	}
}

func TestRelayDeliversAndSigns(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	rec := &hookRecorder{status: []int{200}}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	endpoint, err := repo.CreateEndpoint(ctx, merchant, server.URL+"/hook")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	if _, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "relay-order-key", "rhash-order"); err != nil {
		t.Fatalf("order: %v", err)
	}

	settled, err := repo.ProcessDueDeliveries(ctx, http.DefaultClient)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}

	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	if len(rec.bodies) != 1 {
		t.Fatalf("received %d posts, want 1", len(rec.bodies))
	}

	if !webhooks.Verify(endpoint.Secret, rec.bodies[0], rec.signals[0]) {
		t.Fatal("delivery signature does not verify")
	}

	if rec.ids[0] == "" {
		t.Fatal("delivery lacks X-Minipay-Event-ID")
	}

	var status string

	err = pool.QueryRow(ctx, `
		SELECT status FROM deliveries d
		JOIN events e ON e.id = d.event_id
		WHERE e.merchant_id = $1
	`, merchant).Scan(&status)
	if err != nil {
		t.Fatalf("delivery row: %v", err)
	}

	if status != "SUCCEEDED" {
		t.Fatalf("delivery status = %q, want SUCCEEDED", status)
	}
}

func TestRelayRetriesThenSucceeds(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	rec := &hookRecorder{status: []int{500, 500, 200}}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	if _, err := repo.CreateEndpoint(ctx, merchant, server.URL+"/hook"); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	if _, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "retry-order-key", "rhash-retry-order"); err != nil {
		t.Fatalf("order: %v", err)
	}

	travel := func() {
		_, err := pool.Exec(ctx, `
			UPDATE deliveries SET next_attempt_at = now()
			WHERE id IN (
				SELECT d.id FROM deliveries d
				JOIN events e ON e.id = d.event_id
				WHERE e.merchant_id = $1
			)
		`, merchant)
		if err != nil {
			t.Fatalf("time travel: %v", err)
		}
	}

	deliveryState := func() (string, int) {
		var status string
		var attempts int

		err := pool.QueryRow(ctx, `
			SELECT d.status, d.attempts FROM deliveries d
			JOIN events e ON e.id = d.event_id
			WHERE e.merchant_id = $1
		`, merchant).Scan(&status, &attempts)
		if err != nil {
			t.Fatalf("delivery state: %v", err)
		}

		return status, attempts
	}

	// Attempt 1 fails: still pending, backed off into the future.
	if settled, err := repo.ProcessDueDeliveries(ctx, http.DefaultClient); err != nil || settled != 0 {
		t.Fatalf("run 1: settled=%d err=%v, want 0 nil", settled, err)
	}

	if status, attempts := deliveryState(); status != "PENDING" || attempts != 1 {
		t.Fatalf("after run 1: (%q, %d), want (PENDING, 1)", status, attempts)
	}

	// Attempt 2 fails the same way.
	travel()

	if settled, err := repo.ProcessDueDeliveries(ctx, http.DefaultClient); err != nil || settled != 0 {
		t.Fatalf("run 2: settled=%d err=%v, want 0 nil", settled, err)
	}

	if status, attempts := deliveryState(); status != "PENDING" || attempts != 2 {
		t.Fatalf("after run 2: (%q, %d), want (PENDING, 2)", status, attempts)
	}

	// Attempt 3 succeeds.
	travel()

	if settled, err := repo.ProcessDueDeliveries(ctx, http.DefaultClient); err != nil || settled != 1 {
		t.Fatalf("run 3: settled=%d err=%v, want 1 nil", settled, err)
	}

	if status, attempts := deliveryState(); status != "SUCCEEDED" || attempts != 3 {
		t.Fatalf("after run 3: (%q, %d), want (SUCCEEDED, 3)", status, attempts)
	}
}

func TestRelayFailsDeactivatedEndpoint(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	endpoint, err := repo.CreateEndpoint(ctx, merchant, "http://localhost:9/hook")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	if _, _, err := repo.CreateIdempotent(ctx, Order{
		ID:         testUUID(t),
		MerchantID: merchant,
		Amount:     50000,
		Currency:   "INR",
		Status:     "CREATED",
	}, "dead-order-key", "rhash-dead-order"); err != nil {
		t.Fatalf("order: %v", err)
	}

	if err := repo.DeactivateEndpoint(ctx, merchant, endpoint.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	settled, err := repo.ProcessDueDeliveries(ctx, http.DefaultClient)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}

	if settled != 1 {
		t.Fatalf("settled = %d, want 1 (failed, terminal)", settled)
	}

	var status string

	err = pool.QueryRow(ctx, `
		SELECT d.status FROM deliveries d
		JOIN events e ON e.id = d.event_id
		WHERE e.merchant_id = $1
	`, merchant).Scan(&status)
	if err != nil {
		t.Fatalf("delivery row: %v", err)
	}

	if status != "FAILED" {
		t.Fatalf("delivery status = %q, want FAILED", status)
	}
}

func TestListEvents(t *testing.T) {
	pool := testPool(t)
	repo := &OrderRepository{DB: pool}
	ctx := context.Background()
	merchant := "test-merchant-" + testUUID(t)[:8]
	cleanupMerchant(t, repo, merchant)

	for _, key := range []string{"ev-order-a", "ev-order-b"} {
		if _, _, err := repo.CreateIdempotent(ctx, Order{
			ID:         testUUID(t),
			MerchantID: merchant,
			Amount:     1000,
			Currency:   "INR",
			Status:     "CREATED",
		}, key, "ehash-"+key); err != nil {
			t.Fatalf("seed order: %v", err)
		}
	}

	all, more, err := repo.ListEvents(ctx, merchant, 20, "", "")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	if len(all) != 2 || more {
		t.Fatalf("events: got %d more=%v, want 2 false", len(all), more)
	}

	if all[0].Type != EventOrderCreated || all[0].Payload == nil {
		t.Fatalf("event shape: %+v", all[0])
	}

	filtered, _, err := repo.ListEvents(ctx, merchant, 20, "", EventOrderCreated)
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}

	if len(filtered) != 2 {
		t.Fatalf("filtered: got %d, want 2", len(filtered))
	}

	none, _, err := repo.ListEvents(ctx, merchant, 20, "", EventPaymentCaptured)
	if err != nil {
		t.Fatalf("empty filter: %v", err)
	}

	if len(none) != 0 {
		t.Fatalf("empty filter: got %d, want 0", len(none))
	}

	paged, more, err := repo.ListEvents(ctx, merchant, 1, "", "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}

	if len(paged) != 1 || !more {
		t.Fatalf("page 1: got %d more=%v, want 1 true", len(paged), more)
	}

	page2, more2, err := repo.ListEvents(ctx, merchant, 1, paged[0].ID, "")
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	if len(page2) != 1 || more2 || page2[0].ID == paged[0].ID {
		t.Fatalf("page 2: unexpected %+v more=%v", page2, more2)
	}

	if _, _, err := repo.ListEvents(ctx, merchant, 20, "", "order.shipped"); !errors.Is(err, ErrInvalidEventType) {
		t.Fatalf("bad type: got %v, want ErrInvalidEventType", err)
	}

	if _, _, err := repo.ListEvents(ctx, merchant, 20, testUUID(t), ""); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor: got %v, want ErrInvalidCursor", err)
	}
}
