-- API-key authentication.
--
-- merchants holds the canonical merchant registry. orders, payments, and
-- the idempotency key tables keep merchant_id as free text (scoped
-- namespacing, deliberately NOT a foreign key: tests and historical rows
-- use ad-hoc merchant names that must keep working).
--
-- api_keys stores only SHA256 hashes — raw keys are shown once at issuance
-- and never again. revoked_at soft-revokes without breaking audit trails.

CREATE TABLE IF NOT EXISTS merchants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO merchants (id, name)
VALUES ('demo-merchant', 'Demo Merchant')
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS api_keys (
    id          UUID PRIMARY KEY,
    merchant_id TEXT NOT NULL REFERENCES merchants (id),
    key_hash    TEXT NOT NULL UNIQUE,
    prefix      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_keys_merchant
    ON api_keys (merchant_id);
