package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"minipay/internal/payment"
	"minipay/internal/store"
)

type CreateRefundRequest struct {
	Amount int64 `json:"amount"`
}

type RefundHandler struct {
	Service *payment.OrderService
}

func (h *RefundHandler) CreateRefund(
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

	// Extract the payment ID from the URL.
	const prefix = "/v1/payments/"
	const suffix = "/refunds"

	path := strings.TrimPrefix(r.URL.Path, prefix)

	if !strings.HasSuffix(path, suffix) {
		http.NotFound(w, r)
		return
	}

	paymentID := strings.TrimSuffix(path, suffix)

	if paymentID == "" || strings.Contains(paymentID, "/") {
		http.Error(
			w,
			"invalid payment ID",
			http.StatusBadRequest,
		)
		return
	}

	// Decode the request body.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateRefundRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(
			w,
			"unexpected trailing JSON",
			http.StatusBadRequest,
		)
		return
	}

	if req.Amount <= 0 {
		http.Error(
			w,
			"refund amount must be positive",
			http.StatusBadRequest,
		)
		return
	}

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(
			w,
			"Idempotency-Key header is required",
			http.StatusBadRequest,
		)
		return
	}

	refund, created, err := h.Service.RefundPayment(
		ctx,
		"demo-merchant",
		paymentID,
		req.Amount,
		key,
	)

	if err != nil {
		switch {
		case errors.Is(err, payment.ErrInvalidAmount),
			errors.Is(err, payment.ErrAmountTooLarge),
			errors.Is(err, payment.ErrInvalidPaymentID),
			errors.Is(err, payment.ErrMissingIdempotencyKey),
			errors.Is(err, payment.ErrIdempotencyKeyTooLong):
			http.Error(
				w,
				err.Error(),
				http.StatusBadRequest,
			)

		case errors.Is(err, store.ErrRefundNotAllowed),
			errors.Is(err, store.ErrIdempotencyConflict):
			http.Error(
				w,
				err.Error(),
				http.StatusConflict,
			)

		default:
			log.Printf("create refund failed: %v", err)
			http.Error(
				w,
				"could not create refund",
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

	if err := json.NewEncoder(w).Encode(refund); err != nil {
		log.Printf("encode refund response: %v", err)
	}
}
