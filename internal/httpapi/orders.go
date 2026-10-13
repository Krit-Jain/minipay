package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"minipay/internal/payment"
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateOrderRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")
		return
	}

	// Require exactly one JSON value.
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

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	order, created, err := h.Service.CreateOrder(
		ctx,
		MerchantFromContext(r.Context()),
		req.Amount,
		req.Currency,
		key,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("create order failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
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

	if err := json.NewEncoder(w).Encode(order); err != nil {
		slog.Error("encode order response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *OrderHandler) GetOrder(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	order, err := h.Service.GetOrder(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("get order failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(order); err != nil {
		slog.Error("encode order response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *OrderHandler) ListOrders(
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

	orders, hasMore, err := h.Service.ListOrders(
		ctx,
		MerchantFromContext(r.Context()),
		cursor,
		limit,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("list orders failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	writeList(w, r, orders, hasMore)
}
