package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"minipay/internal/payment"
	"minipay/internal/store"
	"net/http"
	"time"
)

type CreateOrderRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type OrderHandler struct {
	Service *payment.OrderService
}

func (h *OrderHandler) CreateOrder(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodPost {
		http.Error(
			w,
			"method not allowed",
			http.StatusMethodNotAllowed,
		)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateOrderRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Require exactly one JSON value.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(
			w,
			"unexpected trailing JSON",
			http.StatusBadRequest,
		)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(
			w,
			"Idempotency-Key header is required",
			http.StatusBadRequest,
		)
		return
	}

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	order, created, err := h.Service.CreateOrder(
		ctx,
		"demo-merchant",
		req.Amount,
		req.Currency,
		key,
	)

	if err != nil {
		switch {
		case errors.Is(err, payment.ErrInvalidAmount),
			errors.Is(err, payment.ErrUnsupportedCurrency),
			errors.Is(err, payment.ErrMissingIdempotencyKey):
			http.Error(w, err.Error(), http.StatusBadRequest)

		case errors.Is(err, store.ErrIdempotencyConflict):
			http.Error(
				w,
				err.Error(),
				http.StatusConflict,
			)

		default:
			log.Printf("create order failed: %v", err)
			http.Error(
				w,
				"could not create order",
				http.StatusInternalServerError,
			)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if err := json.NewEncoder(w).Encode(order); err != nil {
		log.Printf("encode order response: %v", err)
	}
}
