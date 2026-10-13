package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// requestIDCtxKey carries the per-request ID to access logs and error logs.
type requestIDCtxKey struct{}

// RequestIDFromContext returns the ID assigned by RequestIDMiddleware, or
// "" when the request bypassed it.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDCtxKey{}).(string)

	return id
}

// RequestIDMiddleware tags every request with a random ID, returned to the
// caller as X-Request-ID so clients can quote it in bug reports and we can
// correlate it with server logs.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte

		if _, err := rand.Read(b[:]); err != nil {
			next.ServeHTTP(w, r)
			return
		}

		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(
			r.Context(),
			requestIDCtxKey{},
			id,
		)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// LoggingMiddleware writes one structured access line per request with the
// fields needed to debug production: who, what, how long, what happened.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		slog.Info("request",
			"request_id", RequestIDFromContext(r.Context()),
			"merchant", MerchantFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(),
		)
	})
}

// RateLimiter is a per-key token bucket: each key sustains rate tokens per
// second with short bursts up to burst. It is deliberately dependency-free
// and sized for this service's scale; a Redis cell would replace it if
// MiniPay ever ran behind multiple replicas.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   int
	sweptAt time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(perMinute int, burst int) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perMinute) / 60,
		burst:   burst,
		sweptAt: time.Now(),
	}
}

// Allow consumes one token for key when available. Idle buckets are swept
// at most once a minute so the map cannot grow with one-off clients.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.sweptAt) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 2*time.Minute {
				delete(l.buckets, k)
			}
		}

		l.sweptAt = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}

	b.last = now

	if b.tokens < 1 {
		retryAfter := time.Duration(
			math.Ceil((1-b.tokens)/l.rate),
		) * time.Second

		return false, retryAfter
	}

	b.tokens--

	return true, 0
}

// Limit rejects requests over budget with 429 and a Retry-After hint.
// keyOf selects the budget owner: client IP for the outer layer, merchant
// ID once authenticated.
func Limit(
	limiter *RateLimiter,
	keyOf func(*http.Request) string,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retryAfter := limiter.Allow(keyOf(r))
			if !ok {
				seconds := int64(retryAfter / time.Second)
				if seconds < 1 {
					seconds = 1
				}

				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
				http.Error(
					w,
					"rate limit exceeded",
					http.StatusTooManyRequests,
				)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP extracts the caller address for global rate limiting, preferring
// X-Forwarded-For when behind a proxy.
func ClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return forwarded
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
