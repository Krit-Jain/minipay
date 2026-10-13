# MiniPay

Minimal payments backend in Go (tiny Stripe/Razorpay clone).
Stdlib `net/http` + Postgres via `pgxpool`. No web framework.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/api/main.go` | Wiring: config (`PORT`), JSON logs, DB pool → repo → service → handlers → layered mux (probes public, API behind request-ID + logging + rate limits + auth), graceful shutdown |
| `internal/httpapi/middleware.go` | `X-Request-ID` tagging, structured access logs, per-IP + per-merchant token-bucket rate limiting |
| `cmd/apikey/main.go` | CLI that mints secret API keys (`go run ./cmd/apikey -merchant acme`) |
| `internal/httpapi/` | Auth middleware + context (`auth.go`), validation, JSON parsing, status-code mapping |
| `internal/payment/` | Business rules: amount > 0, `INR` only, `Idempotency-Key` required, UUIDv4 + SHA256 request hash |
| `internal/store/` | Postgres transactions (idempotent orders/payments/refunds, key issuance + lookup in `apikeys.go`) |
| `migrations/` | Authoritative schema, applied in order (`0001`–`0004`) |
| `internal/payment/service_test.go` | Unit tests (no DB needed) |
| `internal/store/store_test.go` | DB integration tests (need `DATABASE_URL`, skip otherwise) |

## Amounts

Minor units as `int64` — paise for `INR`. Never floats. `100` = ₹1.00.

## Auth

Every request needs `Authorization: Bearer <key>`, minted per merchant:

```bash
DATABASE_URL=... go run ./cmd/apikey -merchant acme
# api key (shown once, store it now): sk_test_...
```

Only key hashes are stored; raw keys can't be recovered. Missing, unknown,
or revoked keys get `401` before any handler runs. All idempotency keys and
reads are scoped to the authenticated merchant — one merchant cannot see
or touch another's orders, payments, or refunds (foreign ids read as
`404`, foreign refunds fail as `409`).

The examples below assume `KEY=<your key>` and pass
`-H "Authorization: Bearer $KEY"` on every call.

## API

### `POST /v1/orders`

Header `Idempotency-Key: <opaque client key>` (required, ≤255 chars).

```bash
curl -X POST localhost:8080/v1/orders \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-123' \
  -d '{"amount":50000,"currency":"INR"}'
```

Responses: `201` created, `200` idempotent replay (same key + same body),
`400` validation, `409` same key with different body, `500` server error.

### `GET /v1/orders/{id}`

Returns the order (`200`), or `404` for unknown/malformed ids. The `status`
field shows the lifecycle: `CREATED` → `SUCCEEDED` once paid.

```bash
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/orders/<order-uuid>
```

### `POST /v1/orders/{id}/payments`

Creates a mock-approved (`SUCCEEDED`) payment for a `CREATED` order and
flips the order to `SUCCEEDED` in the same transaction. Header
`Idempotency-Key` required — replays return `200` with the original
payment instead of charging twice. The amount must equal the order amount.

```bash
curl -X POST localhost:8080/v1/orders/<order-uuid>/payments \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: pay-123' \
  -d '{"amount":50000}'
```

Responses: `201` created, `200` replay, `400` bad amount / amount mismatch,
`404` order not found, `409` order already paid or key reused with a
different body, `500` otherwise.

How idempotency works (`store/orders.go`): `INSERT ... ON CONFLICT DO NOTHING`
into `idempotency_keys(merchant_id, key, request_hash)`, then
`SELECT ... FOR UPDATE`, compare hashes, return existing order or insert
order + link key in the same transaction.

### `POST /v1/payments/{id}/refunds`

Header `Idempotency-Key: <opaque client key>` (required, ≤255 chars).

```bash
curl -X POST localhost:8080/v1/payments/<payment-uuid>/refunds \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: refund-123' \
  -d '{"amount":10000}'
```

Responses: `201` created (`PENDING`), `200` idempotent replay (same key +
same body), `400` bad amount/ID/missing key, `409` same key with different
body, or payment missing / not `SUCCEEDED` / insufficient refundable
balance, `500` otherwise.

Anti-over-refund (`store/refunds.go`): the refund row is inserted first
with `balance_reserved = false`, then a single conditional
`UPDATE payments SET refunded_amount += $1 WHERE status='SUCCEEDED' AND ...`
reserves the balance — losers get zero rows → `409`. The `balance_reserved`
flag plus the `UNIQUE (payment_id, idempotency_key)` index let a replay
after a mid-refund crash converge without double-reserving or losing the
reservation. Key, reservation, and record commit in one transaction.

### `GET /v1/payments/{id}`

Returns the payment with its `refunded_amount` (`200`), or `404`.
Payments are scoped to the merchant through their order.

```bash
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/payments/<payment-uuid>
```

## Lifecycle

```
POST /v1/orders → CREATED --POST /v1/orders/{id}/payments--> SUCCEEDED
                                                          --POST .../refunds--> refunded (partially or fully)
```

Only `SUCCEEDED` payments can be refunded, and only up to their unrefunded
balance. A paid order rejects further payments with `409`.

## Run

```bash
cp .env.example .env          # set DATABASE_URL (and optional PORT), then export them
make migrate                  # applies migrations/*.sql in order
go run ./cmd/api              # listens on :8080, or $PORT
```

`GET /healthz` (liveness, always `200`) and `GET /readyz` (`200` only
while Postgres is reachable) are public — no API key needed. The server
drains in-flight requests for up to 10s on `SIGINT`/`SIGTERM`.

Budgets: 600 req/min per client IP globally, 120 req/min per merchant
past auth (bursts 100 / 30). Over budget returns `429` with `Retry-After`.
Every response carries `X-Request-ID`, mirrored in the JSON access logs.

## Test

```bash
make test                     # vet + full suite (DB tests skip without DATABASE_URL)
DATABASE_URL=... go test ./internal/store/ -v   # DB integration tests
```

Unit tests cover validation, hashing, UUID shape, and auth rejection
without a database. Store integration tests cover idempotent order replay
+ conflict + crash-orphan adoption, the payment lifecycle + rejections,
refund replay + conflict + atomic over-refund rejection, and key
issuance / authentication / revocation.

## Known limitations

1. `INR` only; no multi-currency support yet.
2. Payments are mock-approved synchronously — no authorize/capture split,
   no failed-payment path yet.
3. Rate limits are in-memory per instance — a multi-replica deploy would
   need a shared budget store.
