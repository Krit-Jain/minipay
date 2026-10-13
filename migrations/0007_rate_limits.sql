-- Shared rate-limit budgets.
--
-- The per-merchant token buckets live here instead of process memory so
-- every replica enforces the same quota: a merchant bursting across three
-- instances still sees one budget. Keys are merchant IDs; rows are created
-- lazily on first use. The global per-IP limiter stays in memory (cheap
-- flood protection needs no exactness).
CREATE TABLE IF NOT EXISTS rate_limits (
    key        TEXT PRIMARY KEY,
    tokens     DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
