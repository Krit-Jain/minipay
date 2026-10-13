-- Authorize/capture split.
--
-- Payments are no longer born SUCCEEDED: creation only AUTHORIZEs (holds
-- the funds), and a separate capture settles them. Orders therefore need
-- an AUTHORIZED state between CREATED and SUCCEEDED; voiding an
-- authorization returns the order to CREATED so it can be paid again.
-- payments.status already permits all four states, so only the orders
-- CHECK changes. DROP + ADD keeps this file re-runnable.

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_status_check;

ALTER TABLE orders
    ADD CONSTRAINT orders_status_check
    CHECK (status IN ('CREATED', 'AUTHORIZED', 'SUCCEEDED', 'CANCELLED'));
