package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
		MerchantFromContext(r.Context()),
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
			slog.Error("create payment failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
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
		slog.Error("encode payment response", "request_id", RequestIDFromContext(r.Context()), "err", err)
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
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
	)

	if err != nil {
		switch {
		case errors.Is(err, store.ErrPaymentNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)

		default:
			slog.Error("get payment failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
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
		slog.Error("encode payment response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *PaymentHandler) CapturePayment(
	w http.ResponseWriter,
	r *http.Request,
) {
	h.transition(w, r, "capture")
}

func (h *PaymentHandler) VoidPayment(
	w http.ResponseWriter,
	r *http.Request,
) {
	h.transition(w, r, "void")
}

// transition runs capture/void, which share everything except the service
// call and the conflict errors: both are replay-safe 200s, 404 on unknown
// ids, 409 on illegal states.
func (h *PaymentHandler) transition(
	w http.ResponseWriter,
	r *http.Request,
	action string,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	var (
		payment store.Payment
		err     error
	)

	if action == "capture" {
		payment, err = h.Service.CapturePayment(
			ctx,
			MerchantFromContext(r.Context()),
			r.PathValue("id"),
		)
	} else {
		payment, err = h.Service.VoidPayment(
			ctx,
			MerchantFromContext(r.Context()),
			r.PathValue("id"),
		)
	}

	if err != nil {
		switch {
		case errors.Is(err, store.ErrPaymentNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)

		case errors.Is(err, store.ErrPaymentNotCapturable),
			errors.Is(err, store.ErrPaymentNotVoidable):
			http.Error(
				w,
				err.Error(),
				http.StatusConflict,
			)

		default:
			slog.Error(action+" payment failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
			http.Error(
				w,
				"could not "+action+" payment",
				http.StatusInternalServerError,
			)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(payment); err != nil {
		slog.Error("encode payment response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}
