package payment

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	s := &OrderService{Repo: nil} // amount check runs before any DB access
	ctx := context.Background()

	if _, err := s.RefundPayment(ctx, "some-id", 0); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero refund: got %v, want ErrInvalidAmount", err)
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
		if _, err := s.RefundPayment(ctx, bad, 100); !errors.Is(err, ErrInvalidPaymentID) {
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

	if _, err := s.RefundPayment(ctx, validID, MaxAmount+1); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("oversize refund: got %v, want ErrAmountTooLarge", err)
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
