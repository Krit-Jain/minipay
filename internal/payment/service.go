package payment

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"minipay/internal/store"
)

var ErrInvalidAmount = errors.New("amount must be positive")

var ErrUnsupportedCurrency = errors.New("unsupported currency")

type OrderService struct {
	Repo *store.OrderRepository
}

func newID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	// Set UUID version 4 and variant bits.
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

func (s *OrderService) CreateOrder(
	ctx context.Context,
	merchantID string,
	amount int64,
	currency string,
) (store.Order, error) {
	if amount <= 0 {
		return store.Order{}, ErrInvalidAmount
	}

	if currency != "INR" {
		return store.Order{}, ErrUnsupportedCurrency
	}

	id, err := newID()
	if err != nil {
		return store.Order{}, err
	}

	order := store.Order{
		ID:         id,
		MerchantID: merchantID,
		Amount:     amount,
		Currency:   currency,
		Status:     "CREATED",
	}

	if err := s.Repo.Create(ctx, order); err != nil {
		return store.Order{}, err
	}

	return order, nil
}
