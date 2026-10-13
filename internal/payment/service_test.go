package payment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"minipay/internal/store"
)

func TestCreateOrderValidation(t *testing.T) {
	s := &OrderService{Repo: nil} // validation runs before any DB access
	ctx := context.Background()

	if _, _, err := s.CreateOrder(ctx, "m1", 0, "INR", "k1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero amount: got %v, want ErrInvalidAmount", err)
	}

	if _, _, err := s.CreateOrder(ctx, "m1", -5, "INR", "k1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative amount: got %v, want ErrInvalidAmount", err)
	}

	if _, _, err := s.CreateOrder(ctx, "m1", 100, "USD", "k1"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("USD: got %v, want ErrUnsupportedCurrency", err)
	}

	if _, _, err := s.CreateOrder(ctx, "m1", 100, "INR", ""); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Fatalf("empty key: got %v, want ErrMissingIdempotencyKey", err)
	}
}

func TestRefundPaymentValidation(t *testing.T) {
	s := &OrderService{Repo: nil} // validation runs before any DB access
	ctx := context.Background()

	validID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.RefundPayment(ctx, "m1", "some-id", 0, "k1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero refund: got %v, want ErrInvalidAmount", err)
	}

	if _, _, err := s.RefundPayment(ctx, "m1", validID, 100, ""); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Fatalf("empty key: got %v, want ErrMissingIdempotencyKey", err)
	}

	if _, _, err := s.RefundPayment(ctx, "m1", validID, 100, strings.Repeat("k", 256)); !errors.Is(err, ErrIdempotencyKeyTooLong) {
		t.Fatalf("256-char key: got %v, want ErrIdempotencyKeyTooLong", err)
	}
}

func TestCreateOrderKeyTooLong(t *testing.T) {
	s := &OrderService{Repo: nil}
	ctx := context.Background()

	longKey := strings.Repeat("k", 256)

	if _, _, err := s.CreateOrder(ctx, "m1", 100, "INR", longKey); !errors.Is(err, ErrIdempotencyKeyTooLong) {
		t.Fatalf("256-char key: got %v, want ErrIdempotencyKeyTooLong", err)
	}
}

func TestCreateOrderAmountTooLarge(t *testing.T) {
	s := &OrderService{Repo: nil}
	ctx := context.Background()

	if _, _, err := s.CreateOrder(ctx, "m1", MaxAmount+1, "INR", "k1"); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("oversize amount: got %v, want ErrAmountTooLarge", err)
	}
}

func TestRefundInvalidPaymentID(t *testing.T) {
	s := &OrderService{Repo: nil}
	ctx := context.Background()

	for _, bad := range []string{"", "not-a-uuid", "123", "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"} {
		if _, _, err := s.RefundPayment(ctx, "m1", bad, 100, "k1"); !errors.Is(err, ErrInvalidPaymentID) {
			t.Fatalf("payment ID %q: got %v, want ErrInvalidPaymentID", bad, err)
		}
	}
}

func TestRefundAmountTooLarge(t *testing.T) {
	s := &OrderService{Repo: nil}
	ctx := context.Background()

	validID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.RefundPayment(ctx, "m1", validID, MaxAmount+1, "k1"); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("oversize refund: got %v, want ErrAmountTooLarge", err)
	}
}

func TestHashRefundRequestDeterministic(t *testing.T) {
	paymentID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	h1, err := hashRefundRequest(paymentID, 100)
	if err != nil {
		t.Fatal(err)
	}

	h2, err := hashRefundRequest(paymentID, 100)
	if err != nil {
		t.Fatal(err)
	}

	if h1 != h2 {
		t.Fatal("same input must produce same hash")
	}

	h3, err := hashRefundRequest(paymentID, 200)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h3 {
		t.Fatal("different amount must produce different hash")
	}

	otherID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	h4, err := hashRefundRequest(otherID, 100)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h4 {
		t.Fatal("different payment must produce different hash")
	}
}

func TestCreatePaymentValidation(t *testing.T) {
	s := &OrderService{Repo: nil} // validation runs before any DB access
	ctx := context.Background()

	validID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.CreatePayment(ctx, "m1", validID, 0, "k1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero amount: got %v, want ErrInvalidAmount", err)
	}

	if _, _, err := s.CreatePayment(ctx, "m1", validID, MaxAmount+1, "k1"); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("oversize amount: got %v, want ErrAmountTooLarge", err)
	}

	if _, _, err := s.CreatePayment(ctx, "m1", "not-a-uuid", 100, "k1"); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("bad order ID: got %v, want ErrOrderNotFound", err)
	}

	if _, _, err := s.CreatePayment(ctx, "m1", validID, 100, ""); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Fatalf("empty key: got %v, want ErrMissingIdempotencyKey", err)
	}

	if _, _, err := s.CreatePayment(ctx, "m1", validID, 100, strings.Repeat("k", 256)); !errors.Is(err, ErrIdempotencyKeyTooLong) {
		t.Fatalf("256-char key: got %v, want ErrIdempotencyKeyTooLong", err)
	}
}

func TestCaptureVoidValidation(t *testing.T) {
	s := &OrderService{Repo: nil} // validation runs before any DB access
	ctx := context.Background()

	if _, err := s.CapturePayment(ctx, "m1", "not-a-uuid"); !errors.Is(err, store.ErrPaymentNotFound) {
		t.Fatalf("capture bad ID: got %v, want ErrPaymentNotFound", err)
	}

	if _, err := s.VoidPayment(ctx, "m1", "not-a-uuid"); !errors.Is(err, store.ErrPaymentNotFound) {
		t.Fatalf("void bad ID: got %v, want ErrPaymentNotFound", err)
	}
}

func TestHashPaymentRequestDeterministic(t *testing.T) {
	orderID, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	h1, err := hashPaymentRequest(orderID, 100)
	if err != nil {
		t.Fatal(err)
	}

	h2, err := hashPaymentRequest(orderID, 100)
	if err != nil {
		t.Fatal(err)
	}

	if h1 != h2 {
		t.Fatal("same input must produce same hash")
	}

	h3, err := hashPaymentRequest(orderID, 200)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h3 {
		t.Fatal("different amount must produce different hash")
	}
}

func TestHashRequestDeterministic(t *testing.T) {
	h1, err := hashRequest(100, "INR")
	if err != nil {
		t.Fatal(err)
	}

	h2, err := hashRequest(100, "INR")
	if err != nil {
		t.Fatal(err)
	}

	if h1 != h2 {
		t.Fatal("same input must produce same hash")
	}

	h3, err := hashRequest(200, "INR")
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h3 {
		t.Fatal("different amount must produce different hash")
	}
}

func TestNewIDFormat(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}

	if len(id) != 36 {
		t.Fatalf("uuid length: got %d, want 36 (%q)", len(id), id)
	}

	// Version nibble (char 14) must be '4', variant (char 19) one of 8/9/a/b.
	if id[14] != '4' {
		t.Fatalf("uuid version: got %c, want 4 (%q)", id[14], id)
	}

	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("uuid variant: got %c (%q)", id[19], id)
	}
}
