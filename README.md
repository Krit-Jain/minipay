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
| Orders, payments, refunds | Full intent → hold → settle → refund lifecycle |
| Idempotency | Safe retries on all money endpoints (`201` first, `200` replay, `409` on conflict) |
| Auth | Per-merchant API keys (hashed at rest), full tenant isolation |
| Lists | Cursor-paginated `GET` endpoints for orders, payments, refunds |
| Errors | Machine-readable envelope with stable codes + request IDs |
| Hardening | Rate limits (per-IP + shared per-merchant), probes, graceful shutdown, structured logs |
| Currencies | `INR` (paise) + `USD` (cents), integer minor units only |

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
  -H 'Idempotency-Key: pay-1' -d '{"amount":2500,"method":"card","card_number":"4242424242424242"}' | jq -r .id)

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
| `POST /v1/payments/{id}/disputes` | Open a chargeback hold `{amount, reason}` | `201` / `200` replay |
| `POST /v1/disputes/{id}/resolve` | Settle a dispute `{outcome: won|lost}` | `200` |
| `GET /v1/disputes` | List disputes (paginated) | `200` |
| `GET /v1/disputes/{id}` | Fetch one dispute | `200` |
| `GET /v1/balance` | Net receivable per currency | `200` |
| `GET /v1/events` | Audit trail: outbox events, filterable by `?type=` | `200` |
| `GET /metrics` | Prometheus exposition (public) | `200` |
| `POST /v1/webhook_endpoints` | Register a delivery URL `{url}` (secret shown once) | `201` |
| `GET /v1/webhook_endpoints` | List endpoints (secrets never listed) | `200` |
| `DELETE /v1/webhook_endpoints/{id}` | Deactivate an endpoint | `204` |
| `GET /healthz` | Liveness probe (public) | `200 ok` |
| `GET /readyz` | Readiness probe, pings Postgres (public) | `200` / `503` |

### Payment methods

```bash
curl -X POST localhost:8080/v1/orders/<order-uuid>/payments \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: pay-123' \
  -d '{"amount":50000,"method":"card","card_number":"4242424242424242"}'
```

| Method | Required field | Validation |
|---|---|---|
| `card` | `card_number` | 13–19 digits + Luhn checksum; only the last 4 are stored, never the PAN |
| `upi` | `vpa` | Handle format (`name@bank`); travels in memory only, nothing stored |
| `netbanking` | `bank` | Allowlist: `HDFC`, `ICICI`, `SBIN`, `AXIS`, `KOTAK` |

Bad details are `400` (`unsupported_payment_method`, `invalid_payment_details`).
Well-formed requests reach the mock gateway, which approves everything except two
documented test triggers: card `4000000000000002` → `card_declined`, VPA `fail@upi` →
`upi_declined`. A decline still returns `201`, but with `status: FAILED` and a
`failure_code` — the failed row is the audit trail, and the order stays `CREATED`
so the customer can try again. `FAILED` payments can never be captured or refunded.

### Fees, disputes, and balance

Every capture takes the platform's cut: **200 basis points (2%) + a flat fee**
(300 paise / 30 cents), computed in integer math and stored on the payment:

```bash
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/balance
# {"balances":[{"currency":"INR","net_receivable":38700}]}
```

Net receivable per currency = captured − refunded − disputed holds − fees. Holds,
failures, and other merchants' money never count.

When a customer complains through their bank, open a dispute to withhold the amount
while it is investigated:

```bash
curl -X POST localhost:8080/v1/payments/<payment-uuid>/disputes \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"amount":10000,"reason":"fraud"}'
# {"id":"...","status":"OPEN",...}  (reopening identical terms replays it)

curl -X POST localhost:8080/v1/disputes/<dispute-uuid>/resolve \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"outcome":"lost"}'
```

Reasons: `fraud`, `not_received`, `duplicate`, `cancelled`, `other`. Only `SUCCEEDED`
payments can be disputed and only up to their free balance; one `OPEN` dispute per
payment (a second identical open replays, a differing one conflicts). Resolving `won`
releases the hold; resolving `lost` converts it into settled loss, exactly like a
refund. All three transitions emit webhook events (`dispute.opened/won/lost`).

### Error envelope

Every error shares one shape, so clients can switch on `code` and quote `request_id` to support:

```json
{"error": {"code": "order_not_found", "message": "order not found", "request_id": "c1070bfc879f8888"}}
```

| Status | Codes |
|---|---|
| `400` | `invalid_request`, `invalid_json`, `invalid_amount`, `amount_too_large`, `amount_mismatch`, `unsupported_currency`, `unsupported_payment_method`, `invalid_payment_details`, `invalid_endpoint_url`, `invalid_dispute`, `invalid_event_type`, `missing_idempotency_key`, `invalid_idempotency_key`, `invalid_payment_id`, `invalid_cursor` |
| `401` | `unauthorized` (missing, unknown, or revoked key) |
| `404` | `order_not_found`, `payment_not_found`, `endpoint_not_found`, `dispute_not_found` (foreign tenants' records read as `404`) |
| `409` | `idempotency_conflict`, `order_already_paid`, `refund_not_allowed`, `payment_not_capturable`, `payment_not_voidable`, `dispute_not_allowed`, `dispute_not_open` |
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

### Webhooks

Merchants react to events without polling. Register a URL, and every state change is POSTed
to it as JSON:

```bash
curl -X POST localhost:8080/v1/webhook_endpoints \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"url":"https://shop.example.com/minipay-hook"}'
# {"id":"...","url":"...","secret":"whsec_...","active":true,...}  (secret shown once)
```

Event types: `order.created`, `payment.authorized`, `payment.failed`, `payment.captured`,
`payment.voided`, `refund.created`. Each delivery carries `X-Minipay-Event`,
`X-Minipay-Event-ID`, `X-Minipay-Attempt`, and `X-Minipay-Signature` — the hex HMAC-SHA256
of the raw body under your endpoint secret. Verify it before trusting the payload:

```bash
# pseudocode: recompute over the exact bytes received
hmac_sha256(secret, raw_body) == X-Minipay-Signature
```

Delivery guarantees, honestly stated:

- **At-least-once.** Events are written in the *same transaction* as the state change
  (the outbox pattern), so "captured but no event" is impossible. A crashed relay's
  claimed deliveries expire back into the queue. Dedupe on the event id.
- **Retries with backoff.** 2s, 4s, 8s … capped at 5 minutes, up to 25 attempts before
  a delivery dead-letters as `FAILED` (still queryable).
- **Replay-safe emission.** Idempotent replays never emit twice — only first completion does.
- **Shared work.** The relay claims with `FOR UPDATE SKIP LOCKED`, so any number of API
  replicas split the queue instead of double-sending.

### Audit trail

The outbox *is* the audit log: every mutation wrote exactly one event, atomically with
itself. Read it back, filterable by type:

```bash
curl -H "Authorization: Bearer $KEY" 'localhost:8080/v1/events?type=payment.captured'
```

Money rows are append-mostly (statuses flip forward, records are never deleted — only
idempotency linkage rows are garbage-collected), so the trail reconstructs any state.

```mermaid
sequenceDiagram
    autonumber
    participant M as Money tx
    participant O as Outbox (events + deliveries)
    participant R as Relay (every 2s)
    participant E as Merchant endpoint
    M->>O: INSERT event + deliveries (same COMMIT)
    R->>O: claim due rows SKIP LOCKED + 30s lease
    R->>E: POST body + X-Minipay-Signature
    alt 2xx
        R->>O: SUCCEEDED
    else fail + attempts left
        R->>O: PENDING, backoff 2s→4s→…→5min
    else 25 attempts or endpoint dead
        R->>O: FAILED (dead-letter, still queryable)
    end
```

---

## How it works

### Money lifecycle

```mermaid
flowchart LR
    A[CREATED<br/>order intent] -->|POST .../payments| B[AUTHORIZED<br/>funds held]
    A -->|gateway declines| F[FAILED<br/>order stays CREATED]
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
| Hold lifetime | `HOLD_TTL` | `30m` (authorizations older than this are auto-voided) |
| Key lifetime | `KEY_TTL` | `24h` (settled idempotency keys older than this are deleted) |
| Janitor tick | `JANITOR_INTERVAL` | `1m` |
| Merchant budget | — | 120 req/min, burst 30 (shared in Postgres) |
| IP budget | — | 600 req/min, burst 100 (per instance) |

- **Shutdown:** `SIGINT`/`SIGTERM` drains in-flight requests up to 10s.
- **Logs:** JSON on stdout; every line and every response carries `X-Request-ID`.
- **Metrics:** `GET /metrics` exposes Prometheus RED (rate/errors/duration by route
  pattern — never raw paths, so IDs can't explode cardinality) plus first-completion
  business counters (orders/payments/refunds/disputes created, webhook deliveries
  settled). Point Prometheus at it; alert on 5xx rate and p99 latency.
- **Timeouts:** 5s header / 10s read+write / 60s idle; 5s per-request DB contexts.
- **Boot:** missing `DATABASE_URL` or an invalid `PORT` / duration fails fast with a clear
  error instead of a broken server.
- **Janitor:** every tick one replica wins a Postgres advisory lock and sweeps stale
  authorization holds (order back to `CREATED`, `payment.voided` emitted like a merchant
  void) plus expired idempotency keys. Money rows are never deleted — only linkage rows,
  so a retried key after collection starts a fresh claim instead of a false replay.

## Deploy with Docker

The repo ships a production-like local stack: Postgres, one-shot migrations, then the API.
Boot order is enforced by health checks, not sleeps.

```bash
docker compose up --build
DATABASE_URL=postgres://minipay:minipay@localhost:5432/minipay?sslmode=disable \
  go run ./cmd/apikey -merchant acme
curl localhost:8080/healthz   # ok
```

What each piece does:

- **Dockerfile** — multi-stage build: compiles a static `minipay` binary on
  `golang:1.27-alpine`, then ships only the binary on `alpine:3.20` (no toolchain,
  no shell, no source in the final image).
- **compose.yml** — `db` (Postgres 16 with a data volume and health check),
  `migrate` (applies every file in `migrations/` with `ON_ERROR_STOP`, runs exactly
  once), `api` (starts only after migrations succeed).
- **CI** (`.github/workflows/ci.yml`) — on every push and PR: `go vet`, `go build`,
  a Docker image build, `compose config` validation, all migrations applied to a
  fresh database, then the full suite with a real `DATABASE_URL` so no integration
  test silently skips.

---

## Testing

```bash
go vet ./...                    # static checks
go test ./...                   # unit (no DB) + integration (skips without DATABASE_URL)
DATABASE_URL=... go test ./internal/store/ -v   # DB-backed guarantee tests
```

Three layers: **unit** (validation, hashing, limiter math, middleware, envelope shape,
metrics exposition), **integration** (replay/conflict/orphan-adoption/over-refund/cross-tenant
invisibility against real Postgres), **concurrency proof** (`-race` stampedes asserting
exactly-once), and **E2E smoke** (live server: status codes, envelope codes, paging walks,
limiter trip counts — fixtures deleted and verified gone afterwards). A k6 script
(`load/k6-race.js`) mirrors the stampede for manual runs.

---

## Project structure

```
cmd/api        wiring: config → pool → repo → service → handlers → layered mux
cmd/apikey     CLI that mints secret API keys
internal/httpapi  auth middleware, validation, envelope, pagination, handlers
internal/payment  business rules + validation before any DB access
internal/store    transactional Postgres (orders, payments, refunds, keys, limits)
migrations/    numbered, re-runnable schema (0001–0008)
```

Amounts are integer minor units (paise/cents) — never floats. IDs are app-generated UUIDv4.
Request fingerprints are SHA256. API secrets are stored as hashes only.

---

## Security posture

- Secrets at rest: API keys stored as SHA256 hashes only; webhook secrets retrievable
  (signing requires them) and flagged for KMS envelope encryption in production.
- Card data: only last-4 digits persist; full PANs never reach the database.
- Tenancy: every read and write scoped to the authenticated merchant; foreign ids
  read as `404`, foreign writes fail closed.
- Transport: terminate TLS at the load balancer; the app speaks plain HTTP behind it.
- Rotation without downtime: `revoke` (or delete endpoint) then issue anew; overlapping
  validity is the caller's choice during the switch.
- Blast radius: per-IP + per-merchant rate limits, 1MB bodies, strict JSON, timeouts
  on every hop, least-privilege DB role recommended in production.

## Known limitations

1. One full-amount payment per order — no split/partial payments.
2. Per-IP rate limits are per instance; only merchant budgets are shared.
