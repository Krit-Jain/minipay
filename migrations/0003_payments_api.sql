-- Payment creation API.
--
-- 1. payment_idempotency_keys mirrors the order/refund key tables, so a
--    retried payment creation returns the original payment instead of
--    charging twice (201 first, 200 replay).
-- 2. orders_status_check pins the lifecycle this API drives:
--    CREATED -> SUCCEEDED on payment, CANCELLED reserved for a future
--    cancel endpoint. Existing rows are all CREATED, so the backfill is
--    a no-op.

CREATE TABLE IF NOT EXISTS payment_idempotency_keys (
    merchant_id  TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    payment_id   UUID REFERENCES payments (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key)
);

DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'orders_status_check'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT orders_status_check
            CHECK (status IN ('CREATED', 'SUCCEEDED', 'CANCELLED'));
    END IF;
END $$;
