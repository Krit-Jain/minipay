package store

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresLimiter is a per-key token bucket backed by the rate_limits
// table, so every API replica enforces one shared budget. It mirrors the
// semantics of the in-memory limiter in internal/httpapi: rate tokens per
// second sustained, bursts up to burst.
//
// Two deliberate tradeoffs: every call costs one short transaction (fine at
// this scale; a Redis cell would replace it past it), and a database outage
// fails closed — but the readiness probe already sheds traffic then, and
// every money endpoint needs the database anyway.
type PostgresLimiter struct {
	DB    *pgxpool.Pool
	rate  float64
	burst int
}

func NewPostgresLimiter(
	db *pgxpool.Pool,
	perMinute int,
	burst int,
) *PostgresLimiter {
	return &PostgresLimiter{
		DB:    db,
		rate:  float64(perMinute) / 60,
		burst: burst,
	}
}

func (l *PostgresLimiter) Allow(
	ctx context.Context,
	key string,
) (bool, time.Duration) {
	now := time.Now()

	tx, err := l.DB.Begin(ctx)
	if err != nil {
		return false, time.Second
	}
	defer tx.Rollback(ctx)

	tokens := float64(l.burst)

	var updated time.Time

	err = tx.QueryRow(ctx, `
		SELECT tokens, updated_at
		FROM rate_limits
		WHERE key = $1
		FOR UPDATE
	`, key).Scan(&tokens, &updated)

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, time.Second
	}

	if err == nil {
		tokens += now.Sub(updated).Seconds() * l.rate
		if tokens > float64(l.burst) {
			tokens = float64(l.burst)
		}
	}

	if tokens < 1 {
		_ = tx.Rollback(ctx)

		return false, time.Duration(
			math.Ceil((1-tokens)/l.rate),
		) * time.Second
	}

	tokens--

	_, err = tx.Exec(ctx, `
		INSERT INTO rate_limits (key, tokens, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET
			tokens = EXCLUDED.tokens,
			updated_at = EXCLUDED.updated_at
	`, key, tokens, now)
	if err != nil {
		return false, time.Second
	}

	if err := tx.Commit(ctx); err != nil {
		return false, time.Second
	}

	return true, 0
}
