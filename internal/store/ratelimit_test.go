package store

// DB integration tests for the shared rate-limit budget. They run against
// DATABASE_URL and skip otherwise.

import (
	"context"
	"testing"
	"time"
)

func TestPostgresLimiter(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := "rl-test-" + testUUID(t)[:8]

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM rate_limits WHERE key = $1`, key)
	})

	// 120/min = 2/sec, burst of 2.
	limiter := NewPostgresLimiter(pool, 120, 2)

	if ok, _ := limiter.Allow(ctx, key); !ok {
		t.Fatal("1st request: rejected on a fresh budget")
	}

	if ok, _ := limiter.Allow(ctx, key); !ok {
		t.Fatal("2nd request: rejected inside burst")
	}

	ok, retryAfter := limiter.Allow(ctx, key)
	if ok {
		t.Fatal("3rd immediate request: allowed past burst")
	}

	if retryAfter <= 0 {
		t.Fatal("rejection must carry a positive Retry-After hint")
	}

	// After ~600ms one token (of 2/sec) has refilled.
	time.Sleep(600 * time.Millisecond)

	if ok, _ := limiter.Allow(ctx, key); !ok {
		t.Fatal("post-refill request: rejected")
	}
}
