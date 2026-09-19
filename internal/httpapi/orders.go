package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"minipay/internal/payment"
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

	// Reject trailing JSON values.
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		http.Error(w, "unexpected trailing JSON", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Temporary demo merchant.
	// Later, we'll get this from authenticated merchant identity.
	order, err := h.Service.CreateOrder(
		ctx,
		"demo-merchant",
		req.Amount,
		req.Currency,
	)

	if err != nil {
		switch {
		case errors.Is(err, payment.ErrInvalidAmount),
			errors.Is(err, payment.ErrUnsupportedCurrency):
			http.Error(w, err.Error(), http.StatusBadRequest)
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
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(order); err != nil {
		log.Printf("encode order response: %v", err)
	}
}
