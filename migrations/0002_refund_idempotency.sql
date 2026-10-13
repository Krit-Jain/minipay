-- Refund idempotency + crash recovery for idempotency claims.
--
-- 1. orders.idempotency_key (UNIQUE with merchant_id) lets a replay after a
--    crash between the order INSERT and the key linkage find the orphaned
--    order instead of inserting a duplicate. Existing rows keep NULL keys;
--    NULLs never conflict, so the backfill is a no-op.
-- 2. refund_idempotency_keys mirrors idempotency_keys for refunds, so a
--    retried refund returns the original record (201 first, 200 replay).
-- 3. refunds.idempotency_key (UNIQUE with payment_id) + balance_reserved let
--    a replay after a crash mid-refund converge: a row with
--    balance_reserved = true was fully accounted, one with false still needs
--    its balance reservation. No double reserve, no lost reserve.

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_merchant_idempotency_key
    ON orders (merchant_id, idempotency_key);

ALTER TABLE refunds
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

ALTER TABLE refunds
    ADD COLUMN IF NOT EXISTS balance_reserved BOOLEAN NOT NULL DEFAULT FALSE;

CREATE UNIQUE INDEX IF NOT EXISTS uq_refunds_payment_idempotency_key
    ON refunds (payment_id, idempotency_key);

CREATE TABLE IF NOT EXISTS refund_idempotency_keys (
    merchant_id  TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    refund_id    UUID REFERENCES refunds (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key)
);
