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

type CreateDisputeRequest struct {
	Amount int64  `json:"amount"`
	Reason string `json:"reason"`
}

type ResolveDisputeRequest struct {
	Outcome string `json:"outcome"`
}

type DisputeHandler struct {
	Service *payment.OrderService
}

func (h *DisputeHandler) CreateDispute(
	w http.ResponseWriter,
	r *http.Request,
) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateDisputeRequest

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

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	dispute, created, err := h.Service.OpenDispute(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
		req.Amount,
		req.Reason,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("open dispute failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
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

	if err := json.NewEncoder(w).Encode(dispute); err != nil {
		slog.Error("encode dispute response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *DisputeHandler) ResolveDispute(
	w http.ResponseWriter,
	r *http.Request,
) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req ResolveDisputeRequest

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

	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	dispute, err := h.Service.ResolveDispute(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
		req.Outcome,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("resolve dispute failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(dispute); err != nil {
		slog.Error("encode dispute response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *DisputeHandler) GetDispute(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	dispute, err := h.Service.GetDispute(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("get dispute failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(dispute); err != nil {
		slog.Error("encode dispute response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *DisputeHandler) ListDisputes(
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

	disputes, hasMore, err := h.Service.ListDisputes(
		ctx,
		MerchantFromContext(r.Context()),
		cursor,
		limit,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("list disputes failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	writeList(w, r, disputes, hasMore)
}

func (h *DisputeHandler) GetBalance(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	balances, err := h.Service.GetBalance(
		ctx,
		MerchantFromContext(r.Context()),
	)

	if err != nil {
		slog.Error("get balance failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string]any{
		"balances": balances,
	}); err != nil {
		slog.Error("encode balance response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}
