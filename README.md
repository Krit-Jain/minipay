# MiniPay

![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Postgres](https://img.shields.io/badge/Postgres-16+-4169E1?logo=postgresql&logoColor=white)
![license](https://img.shields.io/badge/license-MIT-green)

A minimal payments backend in Go — a tiny Stripe/Razorpay clone. Stdlib `net/http` + Postgres via
`pgxpool`. No web framework. Every money operation is idempotent, every request is authenticated,
and retries, crashes, and tenant mix-ups can't create or destroy money.

> **Learning project?** Start with [LEARNING.md](LEARNING.md) — the full guide to what was built,
> why each piece exists, and the concepts behind it, with analogies throughout.

---

## Features

| Area | What you get |
|------|--------------|
| 💰 Orders, payments, refunds | Full intent → hold → settle → refund lifecycle |
| 🔁 Idempotency | Safe retries on all money endpoints (`201` first, `200` replay, `409` on conflict) |
| 🔐 Auth | Per-merchant API keys (hashed at rest), full tenant isolation |
| 📄 Lists | Cursor-paginated `GET` endpoints for orders, payments, refunds |
| 🧾 Errors | Machine-readable envelope with stable codes + request IDs |
| 🛡️ Hardening | Rate limits (per-IP + shared per-merchant), probes, graceful shutdown, structured logs |
| 💱 Currencies | `INR` (paise) + `USD` (cents), integer minor units only |

---

## Quickstart

```bash
# 1. Point at Postgres and migrate
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/minipay?sslmode=disable
make migrate                  # applies migrations/*.sql in order (or run them with psql)

# 2. Mint an API key (shown once — store it)
go run ./cmd/apikey -merchant acme
# api key (shown once, store it now): sk_test_...

# 3. Run (listens on :8080, or $PORT)
go run ./cmd/api
```

Charge, capture, and refund $25.00 end to end:

```bash
KEY=sk_test_...
O=$(curl -s -X POST localhost:8080/v1/orders \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1' -d '{"amount":2500,"currency":"USD"}' | jq -r .id)

P=$(curl -s -X POST localhost:8080/v1/orders/$O/payments \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: pay-1' -d '{"amount":2500}' | jq -r .id)

curl -s -X POST localhost:8080/v1/payments/$P/capture -H "Authorization: Bearer $KEY" | jq .status
# "SUCCEEDED"

curl -s -X POST localhost:8080/v1/payments/$P/refunds \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: ref-1' -d '{"amount":1000}' | jq .status
# "PENDING"
```

---

## API reference

All endpoints require `Authorization: Bearer <key>` except the probes. All money writes require an
`Idempotency-Key` header (≤255 chars).

| Method & path | Description | Success |
|---|---|---|
| `POST /v1/orders` | Create an order `{amount, currency}` | `201` / `200` replay |
| `GET /v1/orders` | List orders (paginated) | `200` |
| `GET /v1/orders/{id}` | Fetch one order | `200` |
| `POST /v1/orders/{id}/payments` | Authorize (hold) the order amount | `201` / `200` replay |
| `GET /v1/payments` | List payments (paginated) | `200` |
| `GET /v1/payments/{id}` | Fetch one payment | `200` |
| `POST /v1/payments/{id}/capture` | Settle a hold (no body needed) | `200` |
| `POST /v1/payments/{id}/void` | Release a hold (no body needed) | `200` |
| `POST /v1/payments/{id}/refunds` | Refund captured funds `{amount}` | `201` / `200` replay |
| `GET /v1/payments/{id}/refunds` | List a payment's refunds (paginated) | `200` |
| `GET /healthz` | Liveness probe (public) | `200 ok` |
| `GET /readyz` | Readiness probe, pings Postgres (public) | `200` / `503` |

### Error envelope

Every error shares one shape, so clients can switch on `code` and quote `request_id` to support:

```json
{"error": {"code": "order_not_found", "message": "order not found", "request_id": "c1070bfc879f8888"}}
```

| Status | Codes |
|---|---|
| `400` | `invalid_request`, `invalid_json`, `invalid_amount`, `amount_too_large`, `amount_mismatch`, `unsupported_currency`, `missing_idempotency_key`, `invalid_idempotency_key`, `invalid_payment_id`, `invalid_cursor` |
| `401` | `unauthorized` (missing, unknown, or revoked key) |
| `404` | `order_not_found`, `payment_not_found` (foreign tenants' records read as `404`) |
| `409` | `idempotency_conflict`, `order_already_paid`, `refund_not_allowed`, `payment_not_capturable`, `payment_not_voidable` |
| `429` | `rate_limited` (with `Retry-After`) |
| `500` | `internal_error` (message generic; details in logs under `request_id`) |

### Pagination

Lists return newest-first pages shaped `{"data": [...], "has_more": bool}` (`data` is always an
array, never `null`). Walk pages with `?limit=` (default 20, max 100) and
`?starting_after=<last id>`:

```bash
curl -H "Authorization: Bearer $KEY" 'localhost:8080/v1/orders?limit=2'
curl -H "Authorization: Bearer $KEY" 'localhost:8080/v1/orders?limit=2&starting_after=<last-id>'
```

Keyset pagination ("rows older than this id") is used instead of `OFFSET` so rows created between
page fetches can never shift, skip, or duplicate records.

---

## How it works

### Money lifecycle

```mermaid
flowchart LR
    A[CREATED<br/>order intent] -->|POST .../payments| B[AUTHORIZED<br/>funds held]
    B -->|POST .../capture| C[SUCCEEDED<br/>refundable]
    B -->|POST .../void| D[CANCELLED<br/>order back to CREATED]
    C -->|POST .../refunds| E[Refunded<br/>partially or fully]
```

Only `SUCCEEDED` payments refund (via an atomic balance reservation); a held order rejects new
payments with `409`; voids and captures are replay-safe transitions, and illegal directions
(capture-after-void, void-after-capture) are rejected so money can never silently move or vanish.

### One request's journey

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant M as Mux + middleware
    participant S as Service
    participant D as Postgres (1 tx)
    C->>M: POST /v1/orders + Bearer key + Idempotency-Key
    M->>M: request ID, access log, IP limit, auth, merchant limit
    M->>S: validated amount/currency/key + body hash + UUID
    S->>D: claim key → lock → compare hash
    alt same key + same hash
        D-->>C: 200 original record (retry)
    else same key + different body
        D-->>C: 409 idempotency_conflict
    else new key
        D-->>C: 201 after insert + link + commit
    end
```

### Idempotency under crash

```mermaid
flowchart TD
    R[Retry arrives] --> K{Key linked?}
    K -->|yes| P[Return original · 200]
    K -->|no| H{Hash matches?}
    H -->|no| C[409 conflict]
    H -->|yes| O{Orphan row?}
    O -->|yes| A[Adopt it, link key · 200]
    O -->|no| N[Insert, link, commit · 201]
```

A crash between "write the record" and "link the key" leaves an orphan; unique indexes plus a
`balance_reserved` flag let the replay converge instead of duplicating — no double charge, no lost
reservation. See [LEARNING.md](LEARNING.md) §6 for the full story.

---

## Operations

| Setting | Env | Default |
|---|---|---|
| Database | `DATABASE_URL` | *required* |
| Port | `PORT` | `8080` |
| Merchant budget | — | 120 req/min, burst 30 (shared in Postgres) |
| IP budget | — | 600 req/min, burst 100 (per instance) |

- **Shutdown:** `SIGINT`/`SIGTERM` drains in-flight requests up to 10s.
- **Logs:** JSON on stdout; every line and every response carries `X-Request-ID`.
- **Timeouts:** 5s header / 10s read+write / 60s idle; 5s per-request DB contexts.

---

## Testing

```bash
go vet ./...                    # static checks
go test ./...                   # unit (no DB) + integration (skips without DATABASE_URL)
DATABASE_URL=... go test ./internal/store/ -v   # DB-backed guarantee tests
```

Three layers: **unit** (validation, hashing, limiter math, middleware, envelope shape),
**integration** (replay/conflict/orphan-adoption/over-refund/cross-tenant invisibility against real
Postgres), and **E2E smoke** (live server: status codes, envelope codes, paging walks, limiter trip
counts — fixtures deleted and verified gone afterwards).

---

## Project structure

```
cmd/api        wiring: config → pool → repo → service → handlers → layered mux
cmd/apikey     CLI that mints secret API keys
internal/httpapi  auth middleware, validation, envelope, pagination, handlers
internal/payment  business rules + validation before any DB access
internal/store    transactional Postgres (orders, payments, refunds, keys, limits)
migrations/    numbered, re-runnable schema (0001–0007)
```

Amounts are integer minor units (paise/cents) — never floats. IDs are app-generated UUIDv4.
Request fingerprints are SHA256. API secrets are stored as hashes only.

---

## Roadmap

- [x] **A — Envelope + listings** (this phase): stable error codes, cursor pagination
- [ ] **B — Deployability:** multi-stage Docker, compose, CI, boot validation
- [ ] **C — Gateway realism:** payment methods (card/UPI/netbanking validation) + decline path
- [ ] **D — Webhooks:** transactional outbox, HMAC-signed delivery, retries
- [ ] **E — Fees + disputes:** basis-point math, chargeback lifecycle
- [ ] **F — Janitor jobs:** hold expiry, key GC, advisory-lock election
- [ ] **G — Proof:** Prometheus metrics, audit trail, k6 load tests

## Known limitations

1. Authorizations never expire — a hold stays until captured/voided (Phase F).
2. One full-amount payment per order — no split/partial payments.
3. Per-IP rate limits are per instance; only merchant budgets are shared.
