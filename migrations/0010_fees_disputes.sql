-- Platform fees and disputes.
--
-- payments.fee records the platform's cut (basis points + fixed, computed
-- at capture; see payment.computeFee). payments.disputed_amount tracks
-- funds withheld while a dispute is OPEN; the balance available for new
-- refunds or disputes is amount - refunded - disputed, enforced by the
-- combined CHECK below (existing rows satisfy it: disputed starts at 0).
-- disputes allows one OPEN row per payment (partial unique index): a
-- payment under dispute cannot be disputed twice at once, while sequential
-- disputes stay possible. events gains dispute_id so dispute transitions
-- join the outbox like every other state change.

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS fee BIGINT NOT NULL DEFAULT 0
    CHECK (fee >= 0);

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS disputed_amount BIGINT NOT NULL DEFAULT 0
    CHECK (disputed_amount >= 0);

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_pool_check;

ALTER TABLE payments
    ADD CONSTRAINT payments_pool_check
    CHECK (refunded_amount + disputed_amount <= amount);

CREATE TABLE IF NOT EXISTS disputes (
    id          UUID PRIMARY KEY,
    payment_id  UUID NOT NULL REFERENCES payments (id),
    merchant_id TEXT NOT NULL,
    amount      BIGINT NOT NULL CHECK (amount > 0),
    reason      TEXT NOT NULL
        CHECK (reason IN ('fraud', 'not_received', 'duplicate', 'cancelled', 'other')),
    status      TEXT NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN', 'WON', 'LOST')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_disputes_open_payment
    ON disputes (payment_id) WHERE status = 'OPEN';

CREATE INDEX IF NOT EXISTS idx_disputes_merchant
    ON disputes (merchant_id, created_at DESC);

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS dispute_id UUID NULL REFERENCES disputes (id);
