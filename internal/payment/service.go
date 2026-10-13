package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"minipay/internal/store"
)

var ErrInvalidAmount = errors.New("amount must be positive")

var ErrAmountTooLarge = errors.New("amount exceeds maximum allowed")

var ErrUnsupportedCurrency = errors.New("unsupported currency")

// SupportedCurrencies is the ISO allowlist. Amounts are integer minor units
// per currency (paise for INR, cents for USD) — the set grows by adding a
// code here plus the matching CHECK in migrations/0006_currencies.sql.
var SupportedCurrencies = map[string]bool{
	"INR": true,
	"USD": true,
}

var ErrMissingIdempotencyKey = errors.New(
	"Idempotency-Key header is required",
)

var ErrIdempotencyKeyTooLong = errors.New("Idempotency-Key is too long")

var ErrInvalidPaymentID = errors.New("invalid payment ID")

var ErrUnsupportedPaymentMethod = errors.New("unsupported payment method")

var ErrInvalidPaymentDetails = errors.New("invalid payment details")

const (
	PaymentMethodCard       = "card"
	PaymentMethodUPI        = "upi"
	PaymentMethodNetbanking = "netbanking"
)

// SupportedBanks is the netbanking allowlist. Codes are short names on
// purpose: the mock gateway holds no real bank integrations.
var SupportedBanks = map[string]bool{
	"HDFC":  true,
	"ICICI": true,
	"SBIN":  true,
	"AXIS":  true,
	"KOTAK": true,
}

// Gateway failure codes, stored on FAILED payments and returned as
// failure_code so clients can react programmatically.
const (
	FailureCardDeclined = "card_declined"
	FailureUPIDeclined  = "upi_declined"
)

// Deterministic mock-gateway triggers, documented for integrators: these
// inputs always decline, everything else approves, so both paths are
// walkable without flakiness.
const (
	DeclineTestCard = "4000000000000002"
	DeclineTestVPA  = "fail@upi"
)

// MaxAmount caps order/refund amounts (minor units, paise for INR).
// 1_000_000_000 paise = Rs 1 crore. It guards against absurd inputs and
// keeps refunded_amount + amount far from int64 overflow.
const MaxAmount = 1_000_000_000

// isValidUUID reports whether s has the 8-4-4-4-12 hex shape the app
// generates (see newID) and the payments/refunds tables store as UUID.
// It lets the service reject malformed IDs with 400 before Postgres
// would fail the cast with a 500.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}

	for i := 0; i < 36; i++ {
		c := s[i]

		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := c >= '0' && c <= '9' ||
				c >= 'a' && c <= 'f' ||
				c >= 'A' && c <= 'F'
			if !isHex {
				return false
			}
		}
	}

	return true
}

// validCardNumber checks length, digits, and the Luhn checksum — the same
// first line of defense every gateway runs before contacting a bank.
func validCardNumber(number string) bool {
	digits := make([]byte, 0, len(number))

	for i := 0; i < len(number); i++ {
		c := number[i]

		if c == ' ' {
			continue
		}

		if c < '0' || c > '9' {
			return false
		}

		digits = append(digits, c-'0')
	}

	if len(digits) < 13 || len(digits) > 19 {
		return false
	}

	sum := 0

	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i])

		if (len(digits)-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}

		sum += d
	}

	return sum%10 == 0
}

var vpaPattern = regexp.MustCompile(`^[\w.\-]{2,256}@[a-zA-Z]{2,64}$`)

func validVPA(vpa string) bool {
	return vpaPattern.MatchString(vpa)
}

// mockAuthorize stands in for the acquiring bank. Only the documented test
// triggers decline; everything well-formed approves.
func mockAuthorize(method, cardNumber, vpa string) (bool, string) {
	switch method {
	case PaymentMethodCard:
		if cardNumber == DeclineTestCard {
			return false, FailureCardDeclined
		}
	case PaymentMethodUPI:
		if vpa == DeclineTestVPA {
			return false, FailureUPIDeclined
		}
	}

	return true, ""
}

type OrderService struct {
	Repo *store.OrderRepository
}

// FeeBasisPoints is the platform's percentage cut in basis points
// (100bps = 1%): 200bps = 2%. Integer math only — amount*200/10000 truncates
// fractions of a minor unit in the merchant's favour, deterministically.
const FeeBasisPoints = 200

// FeeFixed is the per-capture flat fee in minor units by currency. Like the
// currency allowlist, it grows by adding an entry here; the store never
// invents fees on its own.
var FeeFixed = map[string]int64{
	"INR": 300,
	"USD": 30,
}

// computeFee returns the platform's cut for a settled payment. It runs on
// immutable fields (amount, currency never change after creation), so the
// service can compute it before the capture transaction without TOCTOU risk.
func computeFee(currency string, amount int64) (int64, error) {
	fixed, ok := FeeFixed[currency]
	if !ok {
		return 0, ErrUnsupportedCurrency
	}

	return amount*FeeBasisPoints/10000 + fixed, nil
}

// CreatePaymentInput bundles payment creation parameters: nine scalars
// would be unreadable at call sites.
type CreatePaymentInput struct {
	MerchantID string
	OrderID    string
	Amount     int64
	Method     string
	CardNumber string
	VPA        string
	Bank       string
	Key        string
}

func newID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	), nil
}

func hashRequest(amount int64, currency string) (string, error) {
	normalized := struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}{
		Amount:   amount,
		Currency: currency,
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func hashRefundRequest(paymentID string, amount int64) (string, error) {
	normalized := struct {
		PaymentID string `json:"payment_id"`
		Amount    int64  `json:"amount"`
	}{
		PaymentID: paymentID,
		Amount:    amount,
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func hashPaymentRequest(orderID string, amount int64, method string) (string, error) {
	normalized := struct {
		OrderID string `json:"order_id"`
		Amount  int64  `json:"amount"`
		Method  string `json:"method"`
	}{
		OrderID: orderID,
		Amount:  amount,
		Method:  method,
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func (s *OrderService) CreateOrder(
	ctx context.Context,
	merchantID string,
	amount int64,
	currency string,
	key string,
) (store.Order, bool, error) {
	if amount <= 0 {
		return store.Order{}, false, ErrInvalidAmount
	}

	if amount > MaxAmount {
		return store.Order{}, false, ErrAmountTooLarge
	}

	if !SupportedCurrencies[currency] {
		return store.Order{}, false, ErrUnsupportedCurrency
	}

	if key == "" {
		return store.Order{}, false, ErrMissingIdempotencyKey
	}

	if len(key) > 255 {
		return store.Order{}, false, ErrIdempotencyKeyTooLong
	}

	requestHash, err := hashRequest(amount, currency)
	if err != nil {
		return store.Order{}, false, err
	}

	id, err := newID()
	if err != nil {
		return store.Order{}, false, err
	}

	order := store.Order{
		ID:         id,
		MerchantID: merchantID,
		Amount:     amount,
		Currency:   currency,
		Status:     "CREATED",
	}

	return s.Repo.CreateIdempotent(
		ctx,
		order,
		key,
		requestHash,
	)
}

func (s *OrderService) RefundPayment(
	ctx context.Context,
	merchantID string,
	paymentID string,
	amount int64,
	key string,
) (store.Refund, bool, error) {
	if amount <= 0 {
		return store.Refund{}, false, ErrInvalidAmount
	}

	if amount > MaxAmount {
		return store.Refund{}, false, ErrAmountTooLarge
	}

	if !isValidUUID(paymentID) {
		return store.Refund{}, false, ErrInvalidPaymentID
	}

	if key == "" {
		return store.Refund{}, false, ErrMissingIdempotencyKey
	}

	if len(key) > 255 {
		return store.Refund{}, false, ErrIdempotencyKeyTooLong
	}

	requestHash, err := hashRefundRequest(paymentID, amount)
	if err != nil {
		return store.Refund{}, false, err
	}

	id, err := newID()
	if err != nil {
		return store.Refund{}, false, err
	}

	return s.Repo.CreateRefundIdempotent(
		ctx,
		paymentID,
		id,
		amount,
		merchantID,
		key,
		requestHash,
	)
}

func (s *OrderService) CreatePayment(
	ctx context.Context,
	in CreatePaymentInput,
) (store.Payment, bool, error) {
	if in.Amount <= 0 {
		return store.Payment{}, false, ErrInvalidAmount
	}

	if in.Amount > MaxAmount {
		return store.Payment{}, false, ErrAmountTooLarge
	}

	if !isValidUUID(in.OrderID) {
		return store.Payment{}, false, store.ErrOrderNotFound
	}

	if in.Key == "" {
		return store.Payment{}, false, ErrMissingIdempotencyKey
	}

	if len(in.Key) > 255 {
		return store.Payment{}, false, ErrIdempotencyKeyTooLong
	}

	switch in.Method {
	case PaymentMethodCard, PaymentMethodUPI, PaymentMethodNetbanking:
		// Supported below; details validated per method.
	default:
		return store.Payment{}, false, ErrUnsupportedPaymentMethod
	}

	last4 := ""

	switch in.Method {
	case PaymentMethodCard:
		if !validCardNumber(in.CardNumber) {
			return store.Payment{}, false, ErrInvalidPaymentDetails
		}

		digits := make([]byte, 0, len(in.CardNumber))

		for i := 0; i < len(in.CardNumber); i++ {
			if in.CardNumber[i] != ' ' {
				digits = append(digits, in.CardNumber[i])
			}
		}

		last4 = string(digits[len(digits)-4:])

	case PaymentMethodUPI:
		if !validVPA(in.VPA) {
			return store.Payment{}, false, ErrInvalidPaymentDetails
		}

	case PaymentMethodNetbanking:
		if !SupportedBanks[in.Bank] {
			return store.Payment{}, false, ErrInvalidPaymentDetails
		}
	}

	requestHash, err := hashPaymentRequest(in.OrderID, in.Amount, in.Method)
	if err != nil {
		return store.Payment{}, false, err
	}

	id, err := newID()
	if err != nil {
		return store.Payment{}, false, err
	}

	// The full PAN / VPA lives only in this function's locals: the store
	// receives at most the last four digits. Card data must never reach
	// the database (PCI scope).
	approved, failureCode := mockAuthorize(in.Method, in.CardNumber, in.VPA)

	payment := store.Payment{
		ID:          id,
		Amount:      in.Amount,
		Method:      in.Method,
		CardLast4:   last4,
		FailureCode: failureCode,
		Status:      "AUTHORIZED",
	}

	if !approved {
		payment.Status = "FAILED"
	}

	return s.Repo.CreatePaymentIdempotent(
		ctx,
		payment,
		in.OrderID,
		in.MerchantID,
		in.Key,
		requestHash,
	)
}

func (s *OrderService) GetOrder(
	ctx context.Context,
	merchantID string,
	id string,
) (store.Order, error) {
	if !isValidUUID(id) {
		return store.Order{}, store.ErrOrderNotFound
	}

	return s.Repo.GetOrder(ctx, merchantID, id)
}

func (s *OrderService) GetPayment(
	ctx context.Context,
	merchantID string,
	id string,
) (store.Payment, error) {
	if !isValidUUID(id) {
		return store.Payment{}, store.ErrPaymentNotFound
	}

	return s.Repo.GetPayment(ctx, merchantID, id)
}

func (s *OrderService) CapturePayment(
	ctx context.Context,
	merchantID string,
	id string,
) (store.Payment, error) {
	if !isValidUUID(id) {
		return store.Payment{}, store.ErrPaymentNotFound
	}

	// Read first, then capture: amount and currency are immutable after
	// creation, so the fee derived here cannot go stale before the
	// capture transaction commits (which re-validates state anyway).
	payment, err := s.Repo.GetPayment(ctx, merchantID, id)
	if err != nil {
		return store.Payment{}, err
	}

	fee, err := computeFee(payment.Currency, payment.Amount)
	if err != nil {
		return store.Payment{}, err
	}

	return s.Repo.CapturePayment(ctx, merchantID, id, fee)
}

func (s *OrderService) VoidPayment(
	ctx context.Context,
	merchantID string,
	id string,
) (store.Payment, error) {
	if !isValidUUID(id) {
		return store.Payment{}, store.ErrPaymentNotFound
	}

	return s.Repo.VoidPayment(ctx, merchantID, id)
}

func (s *OrderService) ListOrders(
	ctx context.Context,
	merchantID string,
	cursor string,
	limit int,
) ([]store.Order, bool, error) {
	if cursor != "" && !isValidUUID(cursor) {
		return nil, false, store.ErrInvalidCursor
	}

	return s.Repo.ListOrders(ctx, merchantID, limit, cursor)
}

func (s *OrderService) ListPayments(
	ctx context.Context,
	merchantID string,
	cursor string,
	limit int,
) ([]store.Payment, bool, error) {
	if cursor != "" && !isValidUUID(cursor) {
		return nil, false, store.ErrInvalidCursor
	}

	return s.Repo.ListPayments(ctx, merchantID, limit, cursor)
}

func (s *OrderService) ListRefunds(
	ctx context.Context,
	merchantID string,
	paymentID string,
	cursor string,
	limit int,
) ([]store.Refund, bool, error) {
	if !isValidUUID(paymentID) {
		return nil, false, store.ErrPaymentNotFound
	}

	if cursor != "" && !isValidUUID(cursor) {
		return nil, false, store.ErrInvalidCursor
	}

	return s.Repo.ListRefunds(ctx, merchantID, paymentID, limit, cursor)
}

func (s *OrderService) CreateEndpoint(
	ctx context.Context,
	merchantID string,
	rawURL string,
) (store.Endpoint, error) {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return store.Endpoint{}, store.ErrInvalidEndpointURL
	}

	return s.Repo.CreateEndpoint(ctx, merchantID, rawURL)
}

func (s *OrderService) ListEndpoints(
	ctx context.Context,
	merchantID string,
) ([]store.Endpoint, error) {
	return s.Repo.ListEndpoints(ctx, merchantID)
}

func (s *OrderService) DeleteEndpoint(
	ctx context.Context,
	merchantID string,
	id string,
) error {
	if !isValidUUID(id) {
		return store.ErrEndpointNotFound
	}

	return s.Repo.DeactivateEndpoint(ctx, merchantID, id)
}

func (s *OrderService) OpenDispute(
	ctx context.Context,
	merchantID string,
	paymentID string,
	amount int64,
	reason string,
) (store.Dispute, bool, error) {
	if !isValidUUID(paymentID) {
		return store.Dispute{}, false, store.ErrPaymentNotFound
	}

	if amount <= 0 || amount > MaxAmount {
		return store.Dispute{}, false, ErrInvalidAmount
	}

	if !store.DisputeReasons[reason] {
		return store.Dispute{}, false, store.ErrInvalidDispute
	}

	id, err := newID()
	if err != nil {
		return store.Dispute{}, false, err
	}

	return s.Repo.OpenDispute(ctx, merchantID, paymentID, id, amount, reason)
}

func (s *OrderService) ResolveDispute(
	ctx context.Context,
	merchantID string,
	id string,
	outcome string,
) (store.Dispute, error) {
	if !isValidUUID(id) {
		return store.Dispute{}, store.ErrDisputeNotFound
	}

	if outcome != store.OutcomeWon && outcome != store.OutcomeLost {
		return store.Dispute{}, store.ErrInvalidDispute
	}

	return s.Repo.ResolveDispute(ctx, merchantID, id, outcome)
}

func (s *OrderService) GetDispute(
	ctx context.Context,
	merchantID string,
	id string,
) (store.Dispute, error) {
	if !isValidUUID(id) {
		return store.Dispute{}, store.ErrDisputeNotFound
	}

	return s.Repo.GetDispute(ctx, merchantID, id)
}

func (s *OrderService) ListDisputes(
	ctx context.Context,
	merchantID string,
	cursor string,
	limit int,
) ([]store.Dispute, bool, error) {
	if cursor != "" && !isValidUUID(cursor) {
		return nil, false, store.ErrInvalidCursor
	}

	return s.Repo.ListDisputes(ctx, merchantID, limit, cursor)
}

func (s *OrderService) GetBalance(
	ctx context.Context,
	merchantID string,
) ([]store.CurrencyNet, error) {
	return s.Repo.MerchantBalance(ctx, merchantID)
}

func (s *OrderService) ListEvents(
	ctx context.Context,
	merchantID string,
	cursor string,
	limit int,
	eventType string,
) ([]store.Event, bool, error) {
	if cursor != "" && !isValidUUID(cursor) {
		return nil, false, store.ErrInvalidCursor
	}

	return s.Repo.ListEvents(ctx, merchantID, limit, cursor, eventType)
}
