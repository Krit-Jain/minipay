package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"minipay/internal/payment"
	"minipay/internal/store"
)

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (string, string, string) {
	t.Helper()

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a JSON envelope: %v (%q)", err, rec.Body.String())
	}

	return body.Error.Code, body.Error.Message, body.Error.RequestID
}

func TestWriteErrorEnvelope(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	rec := httptest.NewRecorder()
	RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusConflict, CodeIdempotencyConflict, "key reused")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}

	code, message, requestID := decodeEnvelope(t, rec)

	if code != CodeIdempotencyConflict || message != "key reused" {
		t.Fatalf("envelope = %q %q, want code+message", code, message)
	}

	if requestID == "" {
		t.Fatal("envelope lacks request_id")
	}

	if requestID != rec.Header().Get("X-Request-ID") {
		t.Fatal("envelope request_id does not match X-Request-ID header")
	}
}

func TestStatusAndCodeMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{payment.ErrInvalidAmount, http.StatusBadRequest, CodeInvalidAmount},
		{payment.ErrMissingIdempotencyKey, http.StatusBadRequest, CodeMissingIdempotencyKey},
		{payment.ErrInvalidPaymentID, http.StatusBadRequest, CodeInvalidPaymentID},
		{store.ErrInvalidCursor, http.StatusBadRequest, CodeInvalidCursor},
		{ErrInvalidPagination, http.StatusBadRequest, CodeInvalidCursor},
		{store.ErrOrderNotFound, http.StatusNotFound, CodeOrderNotFound},
		{store.ErrPaymentNotFound, http.StatusNotFound, CodePaymentNotFound},
		{store.ErrOrderAlreadyPaid, http.StatusConflict, CodeOrderAlreadyPaid},
		{store.ErrIdempotencyConflict, http.StatusConflict, CodeIdempotencyConflict},
		{store.ErrRefundNotAllowed, http.StatusConflict, CodeRefundNotAllowed},
		{store.ErrPaymentNotVoidable, http.StatusConflict, CodePaymentNotVoidable},
		{store.ErrInvalidAPIKey, http.StatusUnauthorized, CodeUnauthorized},
	}

	for _, tc := range cases {
		if status, code := statusAndCode(tc.err); status != tc.status || code != tc.code {
			t.Errorf("%v: got (%d, %q), want (%d, %q)",
				tc.err, status, code, tc.status, tc.code)
		}
	}
}

func TestWriteErrorForHidesInternal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	WriteErrorFor(rec, req, errors.New("boom: connection reset"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	code, message, _ := decodeEnvelope(t, rec)

	if code != CodeInternalError {
		t.Fatalf("code = %q, want internal_error", code)
	}

	if message == "boom" || message == "" {
		t.Fatalf("message %q leaks internals or is empty", message)
	}
}
