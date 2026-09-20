package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"minipay/internal/store"
)

var ErrInvalidAmount = errors.New("amount must be positive")

var ErrUnsupportedCurrency = errors.New("unsupported currency")

var ErrMissingIdempotencyKey = errors.New(
	"Idempotency-Key header is required",
)

type OrderService struct {
	Repo *store.OrderRepository
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

	if currency != "INR" {
		return store.Order{}, false, ErrUnsupportedCurrency
	}

	if key == "" {
		return store.Order{}, false, ErrMissingIdempotencyKey
	}

	if len(key) > 255 {
		return store.Order{}, false, errors.New(
			"Idempotency-Key is too long",
		)
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
