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

	if _, _, err := s.CreateOrder(ctx, "m1", 100, "EUR", "k1"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("EUR: got %v, want ErrUnsupportedCurrency", err)
	}

	if _, _, err := s.CreateOrder(ctx, "m1", 100, "usd", "k1"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("lowercase usd: got %v, want ErrUnsupportedCurrency", err)
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

	base := CreatePaymentInput{
		MerchantID: "m1",
		OrderID:    validID,
		Amount:     100,
		Method:     PaymentMethodCard,
		CardNumber: "4242424242424242",
		Key:        "k1",
	}

	withAmount := base
	withAmount.Amount = 0

	if _, _, err := s.CreatePayment(ctx, withAmount); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero amount: got %v, want ErrInvalidAmount", err)
	}

	withBig := base
	withBig.Amount = MaxAmount + 1

	if _, _, err := s.CreatePayment(ctx, withBig); !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("oversize amount: got %v, want ErrAmountTooLarge", err)
	}

	withBadID := base
	withBadID.OrderID = "not-a-uuid"

	if _, _, err := s.CreatePayment(ctx, withBadID); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("bad order ID: got %v, want ErrOrderNotFound", err)
	}

	withNoKey := base
	withNoKey.Key = ""

	if _, _, err := s.CreatePayment(ctx, withNoKey); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Fatalf("empty key: got %v, want ErrMissingIdempotencyKey", err)
	}

	withLongKey := base
	withLongKey.Key = strings.Repeat("k", 256)

	if _, _, err := s.CreatePayment(ctx, withLongKey); !errors.Is(err, ErrIdempotencyKeyTooLong) {
		t.Fatalf("256-char key: got %v, want ErrIdempotencyKeyTooLong", err)
	}

	withBadMethod := base
	withBadMethod.Method = "cash"

	if _, _, err := s.CreatePayment(ctx, withBadMethod); !errors.Is(err, ErrUnsupportedPaymentMethod) {
		t.Fatalf("cash method: got %v, want ErrUnsupportedPaymentMethod", err)
	}

	withBadCard := base
	withBadCard.CardNumber = "1234"

	if _, _, err := s.CreatePayment(ctx, withBadCard); !errors.Is(err, ErrInvalidPaymentDetails) {
		t.Fatalf("short card: got %v, want ErrInvalidPaymentDetails", err)
	}

	withBadUPI := base
	withBadUPI.Method = PaymentMethodUPI
	withBadUPI.VPA = "not-a-vpa"

	if _, _, err := s.CreatePayment(ctx, withBadUPI); !errors.Is(err, ErrInvalidPaymentDetails) {
		t.Fatalf("bad VPA: got %v, want ErrInvalidPaymentDetails", err)
	}

	withBadBank := base
	withBadBank.Method = PaymentMethodNetbanking
	withBadBank.Bank = "NOPE"

	if _, _, err := s.CreatePayment(ctx, withBadBank); !errors.Is(err, ErrInvalidPaymentDetails) {
		t.Fatalf("bad bank: got %v, want ErrInvalidPaymentDetails", err)
	}
}

func TestLuhnAndVPA(t *testing.T) {
	for _, good := range []string{"4242424242424242", "4000000000000002", "4111 1111 1111 1111"} {
		if !validCardNumber(good) {
			t.Fatalf("card %q: rejected, want accepted", good)
		}
	}

	for _, bad := range []string{"", "1234", "4242424242424243", "abcd", "41111111111111111111"} {
		if validCardNumber(bad) {
			t.Fatalf("card %q: accepted, want rejected", bad)
		}
	}

	for _, good := range []string{"alice@okhdfc", "98765@ybl", "fail@upi"} {
		if !validVPA(good) {
			t.Fatalf("vpa %q: rejected, want accepted", good)
		}
	}

	for _, bad := range []string{"", "no-at-sign", "@bank", "a@b", "x@y!"} {
		if validVPA(bad) {
			t.Fatalf("vpa %q: accepted, want rejected", bad)
		}
	}
}

func TestMockAuthorize(t *testing.T) {
	if ok, code := mockAuthorize(PaymentMethodCard, "4242424242424242", ""); !ok || code != "" {
		t.Fatalf("good card: got (%v, %q), want (true, empty)", ok, code)
	}

	if ok, code := mockAuthorize(PaymentMethodCard, DeclineTestCard, ""); ok || code != FailureCardDeclined {
		t.Fatalf("decline card: got (%v, %q)", ok, code)
	}

	if ok, code := mockAuthorize(PaymentMethodUPI, "", DeclineTestVPA); ok || code != FailureUPIDeclined {
		t.Fatalf("decline vpa: got (%v, %q)", ok, code)
	}

	if ok, _ := mockAuthorize(PaymentMethodNetbanking, "", ""); !ok {
		t.Fatal("netbanking: declined, want approved")
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

	h1, err := hashPaymentRequest(orderID, 100, PaymentMethodCard)
	if err != nil {
		t.Fatal(err)
	}

	h2, err := hashPaymentRequest(orderID, 100, PaymentMethodCard)
	if err != nil {
		t.Fatal(err)
	}

	if h1 != h2 {
		t.Fatal("same input must produce same hash")
	}

	h3, err := hashPaymentRequest(orderID, 200, PaymentMethodCard)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h3 {
		t.Fatal("different amount must produce different hash")
	}

	h4, err := hashPaymentRequest(orderID, 100, PaymentMethodUPI)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h4 {
		t.Fatal("different method must produce different hash")
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

func TestEndpointURLValidation(t *testing.T) {
	s := &OrderService{Repo: nil} // validation runs before any DB access
	ctx := context.Background()

	for _, bad := range []string{
		"",
		"not-a-url",
		"ftp://files.example.com/hook",
		"http://",
		"//missing-scheme.com/hook",
	} {
		if _, err := s.CreateEndpoint(ctx, "m1", bad); !errors.Is(err, store.ErrInvalidEndpointURL) {
			t.Fatalf("url %q: got %v, want ErrInvalidEndpointURL", bad, err)
		}
	}

	if err := s.DeleteEndpoint(ctx, "m1", "not-a-uuid"); !errors.Is(err, store.ErrEndpointNotFound) {
		t.Fatalf("bad endpoint id: got %v, want ErrEndpointNotFound", err)
	}
}
