package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterBurstThenReject(t *testing.T) {
	limiter := NewRateLimiter(60, 5) // 1/sec, burst of 5

	for i := 0; i < 5; i++ {
		if ok, _ := limiter.Allow("tenant"); !ok {
			t.Fatalf("request %d: rejected inside burst", i+1)
		}
	}

	ok, retryAfter := limiter.Allow("tenant")
	if ok {
		t.Fatal("6th immediate request: allowed past burst")
	}

	if retryAfter <= 0 {
		t.Fatal("rejection must carry a positive Retry-After hint")
	}

	// Other keys are unaffected.
	if ok, _ := limiter.Allow("someone-else"); !ok {
		t.Fatal("different key: rejected by another key's usage")
	}
}

func TestLimitMiddlewareSetsRetryAfter(t *testing.T) {
	limiter := NewRateLimiter(60, 1)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Limit(limiter, func(*http.Request) string { return "k" })(next)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))

	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", second.Code)
	}

	if second.Header().Get("Retry-After") == "" {
		t.Fatal("429 must include a Retry-After header")
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	var seen string

	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	RequestIDMiddleware(next).ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("downstream handler saw no request ID")
	}

	if rec.Header().Get("X-Request-ID") != seen {
		t.Fatal("X-Request-ID header does not match the context ID")
	}
}

func TestLoggingMiddlewarePassesThrough(t *testing.T) {
	// Silence access logs: the assertion is status/body passthrough.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("pot"))
	})

	rec := httptest.NewRecorder()
	LoggingMiddleware(next).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "/", nil),
	)

	if rec.Code != http.StatusTeapot || rec.Body.String() != "pot" {
		t.Fatalf("passthrough broken: %d %q", rec.Code, rec.Body.String())
	}
}

func TestClientIP(t *testing.T) {
	proxy := httptest.NewRequest(http.MethodGet, "/", nil)
	proxy.Header.Set("X-Forwarded-For", "203.0.113.7")

	if got := ClientIP(proxy); got != "203.0.113.7" {
		t.Fatalf("forwarded IP = %q, want 203.0.113.7", got)
	}

	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "198.51.100.9:1234"

	if got := ClientIP(direct); got != "198.51.100.9" {
		t.Fatalf("direct IP = %q, want 198.51.100.9", got)
	}
}
