package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"minipay/internal/payment"
	"minipay/internal/store"
)

// Error codes are the machine-readable half of the API contract: clients
// switch on code, humans read message. Codes are stable — renaming one is a
// breaking change — while messages stay free-form.
const (
	CodeInvalidRequest        = "invalid_request"
	CodeInvalidJSON           = "invalid_json"
	CodeInvalidAmount         = "invalid_amount"
	CodeAmountTooLarge        = "amount_too_large"
	CodeAmountMismatch        = "amount_mismatch"
	CodeUnsupportedCurrency   = "unsupported_currency"
	CodeUnsupportedMethod     = "unsupported_payment_method"
	CodeInvalidPaymentDetails = "invalid_payment_details"
	CodeMissingIdempotencyKey = "missing_idempotency_key"
	CodeInvalidIdempotencyKey = "invalid_idempotency_key"
	CodeInvalidPaymentID      = "invalid_payment_id"
	CodeInvalidEndpointURL    = "invalid_endpoint_url"
	CodeEndpointNotFound      = "endpoint_not_found"
	CodeInvalidCursor         = "invalid_cursor"
	CodeNotFound              = "not_found"
	CodeOrderNotFound         = "order_not_found"
	CodePaymentNotFound       = "payment_not_found"
	CodeOrderAlreadyPaid      = "order_already_paid"
	CodeIdempotencyConflict   = "idempotency_conflict"
	CodeRefundNotAllowed      = "refund_not_allowed"
	CodePaymentNotCapturable  = "payment_not_capturable"
	CodePaymentNotVoidable    = "payment_not_voidable"
	CodeUnauthorized          = "unauthorized"
	CodeRateLimited           = "rate_limited"
	CodeInternalError         = "internal_error"
)

// statusAndCode maps domain errors to their HTTP status and stable code.
// Anything unmapped is a server bug: 500 with a generic message so internal
// details never leak, while the request_id lets us find the log line.
func statusAndCode(err error) (int, string) {
	switch {
	case errors.Is(err, payment.ErrInvalidAmount):
		return http.StatusBadRequest, CodeInvalidAmount

	case errors.Is(err, payment.ErrAmountTooLarge):
		return http.StatusBadRequest, CodeAmountTooLarge

	case errors.Is(err, store.ErrPaymentAmountMismatch):
		return http.StatusBadRequest, CodeAmountMismatch

	case errors.Is(err, payment.ErrUnsupportedCurrency):
		return http.StatusBadRequest, CodeUnsupportedCurrency

	case errors.Is(err, payment.ErrUnsupportedPaymentMethod):
		return http.StatusBadRequest, CodeUnsupportedMethod

	case errors.Is(err, payment.ErrInvalidPaymentDetails):
		return http.StatusBadRequest, CodeInvalidPaymentDetails

	case errors.Is(err, payment.ErrMissingIdempotencyKey):
		return http.StatusBadRequest, CodeMissingIdempotencyKey

	case errors.Is(err, payment.ErrIdempotencyKeyTooLong):
		return http.StatusBadRequest, CodeInvalidIdempotencyKey

	case errors.Is(err, payment.ErrInvalidPaymentID):
		return http.StatusBadRequest, CodeInvalidPaymentID

	case errors.Is(err, store.ErrInvalidEndpointURL):
		return http.StatusBadRequest, CodeInvalidEndpointURL

	case errors.Is(err, store.ErrInvalidCursor),
		errors.Is(err, ErrInvalidPagination):
		return http.StatusBadRequest, CodeInvalidCursor

	case errors.Is(err, store.ErrOrderNotFound):
		return http.StatusNotFound, CodeOrderNotFound

	case errors.Is(err, store.ErrPaymentNotFound):
		return http.StatusNotFound, CodePaymentNotFound

	case errors.Is(err, store.ErrEndpointNotFound):
		return http.StatusNotFound, CodeEndpointNotFound

	case errors.Is(err, store.ErrOrderAlreadyPaid):
		return http.StatusConflict, CodeOrderAlreadyPaid

	case errors.Is(err, store.ErrIdempotencyConflict):
		return http.StatusConflict, CodeIdempotencyConflict

	case errors.Is(err, store.ErrRefundNotAllowed):
		return http.StatusConflict, CodeRefundNotAllowed

	case errors.Is(err, store.ErrPaymentNotCapturable):
		return http.StatusConflict, CodePaymentNotCapturable

	case errors.Is(err, store.ErrPaymentNotVoidable):
		return http.StatusConflict, CodePaymentNotVoidable

	case errors.Is(err, store.ErrInvalidAPIKey):
		return http.StatusUnauthorized, CodeUnauthorized

	default:
		return http.StatusInternalServerError, CodeInternalError
	}
}

// WriteError renders the envelope every error response shares:
//
//	{"error": {"code": "...", "message": "...", "request_id": "..."}}
//
// The request_id echoes the X-Request-ID header so a client can quote one
// string and support can find the exact log line.
func WriteError(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	code string,
	message string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    message,
			"request_id": RequestIDFromContext(r.Context()),
		},
	})
}

// WriteErrorFor maps a domain error to its status/code and renders it. The
// error's own message is user-safe by construction (service and store
// errors are written as client-facing sentences); unknown errors collapse
// to a generic 500 so internals never leak.
func WriteErrorFor(w http.ResponseWriter, r *http.Request, err error) {
	status, code := statusAndCode(err)

	message := err.Error()
	if status == http.StatusInternalServerError {
		message = "internal error"
	}

	WriteError(w, r, status, code, message)
}
