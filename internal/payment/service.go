package payment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"minipay/internal/store"
)

var ErrInvalidAmount = errors.New("amount must be positive")

type OrderService struct {
	Repo *store.OrderRepository
}

func newID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
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
		return store.Order{}, errors.New("unsupported currency")
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
