package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"minipay/internal/payment"
	"minipay/internal/store"
)

type CreatePaymentRequest struct {
	Amount int64 `json:"amount"`
}

type PaymentHandler struct {
	Service *payment.OrderService
}

func (h *PaymentHandler) CreatePayment(
	w http.ResponseWriter,
	r *http.Request,
) {
	orderID := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreatePaymentRequest

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

	pay, created, err := h.Service.CreatePayment(
		ctx,
		"demo-merchant",
		orderID,
		req.Amount,
		key,
	)

	if err != nil {
		switch {
		case errors.Is(err, payment.ErrInvalidAmount),
			errors.Is(err, payment.ErrAmountTooLarge),
			errors.Is(err, store.ErrPaymentAmountMismatch):
			http.Error(w, err.Error(), http.StatusBadRequest)

		case errors.Is(err, store.ErrOrderNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)

		case errors.Is(err, store.ErrOrderAlreadyPaid),
			errors.Is(err, store.ErrIdempotencyConflict):
			http.Error(
				w,
				err.Error(),
				http.StatusConflict,
			)

		default:
			log.Printf("create payment failed: %v", err)
			http.Error(
				w,
				"could not create payment",
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

	if err := json.NewEncoder(w).Encode(pay); err != nil {
		log.Printf("encode payment response: %v", err)
	}
}

func (h *PaymentHandler) GetPayment(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	payment, err := h.Service.GetPayment(
		ctx,
		"demo-merchant",
		r.PathValue("id"),
	)

	if err != nil {
		switch {
		case errors.Is(err, store.ErrPaymentNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)

		default:
			log.Printf("get payment failed: %v", err)
			http.Error(
				w,
				"could not fetch payment",
				http.StatusInternalServerError,
			)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(payment); err != nil {
		log.Printf("encode payment response: %v", err)
	}
}
