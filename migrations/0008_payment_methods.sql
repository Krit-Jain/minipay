-- Payment methods and gateway failures.
--
-- method records how the customer paid. Pre-methods rows keep 'unknown'
-- (honest backfill: we genuinely don't know). card_last4 stores only the
-- last four digits — full PANs must never touch this database (PCI scope).
-- failure_code is NULL unless the mock gateway declined the authorization.
-- All three clauses are re-runnable.

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS method TEXT NOT NULL DEFAULT 'unknown';

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_method_check;

ALTER TABLE payments
    ADD CONSTRAINT payments_method_check
    CHECK (method IN ('unknown', 'card', 'upi', 'netbanking'));

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS card_last4 TEXT;

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS failure_code TEXT;
