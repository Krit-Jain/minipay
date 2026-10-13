-- Webhooks via transactional outbox.
--
-- events are written in the SAME transaction as the state change they
-- describe, so a state change without its event (or vice versa) is
-- impossible. deliveries fans each event out to every active endpoint of
-- the merchant; a relay worker claims due rows with FOR UPDATE SKIP LOCKED
-- (safe across replicas) and POSTs them with HMAC signatures.
--
-- merchant_endpoints.secret is stored retrievable (not hashed): the relay
-- needs the raw secret to sign every delivery. A production install should
-- envelope-encrypt it with a KMS; the column exists so that upgrade needs
-- no schema change. endpoint URLs are constrained to http(s).
CREATE TABLE IF NOT EXISTS merchant_endpoints (
    id          UUID PRIMARY KEY,
    merchant_id TEXT NOT NULL,
    url         TEXT NOT NULL CHECK (url LIKE 'http://%' OR url LIKE 'https://%'),
    secret      TEXT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_endpoints_merchant
    ON merchant_endpoints (merchant_id) WHERE active;

CREATE TABLE IF NOT EXISTS events (
    id          UUID PRIMARY KEY,
    merchant_id TEXT NOT NULL,
    type        TEXT NOT NULL,
    order_id    UUID NULL REFERENCES orders (id),
    payment_id  UUID NULL REFERENCES payments (id),
    refund_id   UUID NULL REFERENCES refunds (id),
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_events_merchant_created
    ON events (merchant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS deliveries (
    id              UUID PRIMARY KEY,
    event_id        UUID NOT NULL REFERENCES events (id),
    endpoint_id     UUID NOT NULL REFERENCES merchant_endpoints (id),
    status          TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'SUCCEEDED', 'FAILED')),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);

CREATE INDEX IF NOT EXISTS idx_deliveries_due
    ON deliveries (status, next_attempt_at) WHERE status = 'PENDING';
