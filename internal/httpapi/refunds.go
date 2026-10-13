package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"minipay/internal/payment"
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
	paymentID := r.PathValue("id")

	// Decode the request body.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateRefundRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")
		return
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		WriteError(
			w,
			r,
			http.StatusBadRequest,
			CodeInvalidJSON,
			"unexpected trailing JSON",
		)
		return
	}

	if req.Amount <= 0 {
		WriteError(
			w,
			r,
			http.StatusBadRequest,
			CodeInvalidAmount,
			"refund amount must be positive",
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
		WriteError(
			w,
			r,
			http.StatusBadRequest,
			CodeMissingIdempotencyKey,
			"Idempotency-Key header is required",
		)
		return
	}

	refund, created, err := h.Service.RefundPayment(
		ctx,
		MerchantFromContext(r.Context()),
		paymentID,
		req.Amount,
		key,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("create refund failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if err := json.NewEncoder(w).Encode(refund); err != nil {
		slog.Error("encode refund response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *RefundHandler) ListRefunds(
	w http.ResponseWriter,
	r *http.Request,
) {
	limit, cursor, err := ParsePagination(r)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidCursor, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	refunds, hasMore, err := h.Service.ListRefunds(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
		cursor,
		limit,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("list refunds failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	writeList(w, r, refunds, hasMore)
}
