-- Multi-currency: INR + USD.
--
-- Amounts stay integer minor units in both (paise / cents) — the only rule
-- is "never floats", which holds per currency. The allowlist lives in both
-- layers: the service rejects anything else with 400 before the database
-- CHECK would fire. Existing rows are all INR, so the backfill is a no-op.
-- payments.currency previously had no CHECK; it gains the same list.

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_currency_check;

ALTER TABLE orders
    ADD CONSTRAINT orders_currency_check
    CHECK (currency IN ('INR', 'USD'));

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_currency_check;

ALTER TABLE payments
    ADD CONSTRAINT payments_currency_check
    CHECK (currency IN ('INR', 'USD'));
