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

type CreateEndpointRequest struct {
	URL string `json:"url"`
}

type WebhookHandler struct {
	Service *payment.OrderService
}

func (h *WebhookHandler) CreateEndpoint(
	w http.ResponseWriter,
	r *http.Request,
) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateEndpointRequest

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

	endpoint, err := h.Service.CreateEndpoint(
		ctx,
		MerchantFromContext(r.Context()),
		req.URL,
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("create endpoint failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(endpoint); err != nil {
		slog.Error("encode endpoint response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}

func (h *WebhookHandler) ListEndpoints(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	endpoints, err := h.Service.ListEndpoints(
		ctx,
		MerchantFromContext(r.Context()),
	)

	if err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("list endpoints failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	writeList(w, r, endpoints, false)
}

func (h *WebhookHandler) DeleteEndpoint(
	w http.ResponseWriter,
	r *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		r.Context(),
		5*time.Second,
	)
	defer cancel()

	if err := h.Service.DeleteEndpoint(
		ctx,
		MerchantFromContext(r.Context()),
		r.PathValue("id"),
	); err != nil {
		status, _ := statusAndCode(err)
		if status == http.StatusInternalServerError {
			slog.Error("delete endpoint failed", "request_id", RequestIDFromContext(r.Context()), "err", err)
		}

		WriteErrorFor(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
