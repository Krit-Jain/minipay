package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"minipay/internal/store"
)

// merchantCtxKey carries the authenticated merchant ID down to handlers.
// It is unexported so only this package can read or write it.
type merchantCtxKey struct{}

// MerchantFromContext returns the merchant authenticated by RequireAuth,
// or "" when the request bypassed it (tests, unprotected routes).
func MerchantFromContext(ctx context.Context) string {
	id, _ := ctx.Value(merchantCtxKey{}).(string)

	return id
}

// RequireAuth wraps the API mux and resolves
// "Authorization: Bearer <key>" to a merchant on every request. Missing,
// malformed, unknown, and revoked credentials all fail closed with 401 and
// never reach a handler.
func RequireAuth(
	repo *store.OrderRepository,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := ""

		if scheme, token, found := strings.Cut(
			r.Header.Get("Authorization"),
			" ",
		); found && scheme == "Bearer" {
			raw = token
		}

		if raw == "" {
			WriteError(
				w,
				r,
				http.StatusUnauthorized,
				CodeUnauthorized,
				"missing or invalid Authorization header",
			)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		merchantID, err := repo.Authenticate(ctx, raw)
		if err != nil {
			WriteError(w, r, http.StatusUnauthorized, CodeUnauthorized, "invalid API key")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(
			r.Context(),
			merchantCtxKey{},
			merchantID,
		)))
	})
}
