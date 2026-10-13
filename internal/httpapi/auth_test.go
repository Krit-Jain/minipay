package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// These cases never reach the database: malformed credentials fail before
// any lookup, so a nil repo is safe here. Authenticated paths are covered
// by the store tests and the end-to-end smoke test.
func TestRequireAuthRejectsWithoutLookup(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler must not run for unauthenticated requests")
	})

	handler := RequireAuth(nil, next)

	cases := map[string]string{
		"missing header":   "",
		"malformed scheme": "Token abc",
		"empty bearer":     "Bearer ",
		"bare key":         "sk_test_abc",
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/orders/x", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestMerchantFromContext(t *testing.T) {
	if got := MerchantFromContext(t.Context()); got != "" {
		t.Fatalf("empty context: got %q, want empty", got)
	}
}
