# MiniPay

Minimal payments backend in Go (tiny Stripe/Razorpay clone).
Stdlib `net/http` + Postgres via `pgxpool`. No web framework.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/api/main.go` | Wiring: DB pool → repo → service → handlers → `ServeMux` on `:8080` |
| `internal/httpapi/` | HTTP validation, JSON parsing, status-code mapping (`orders.go`, `refunds.go`) |
| `internal/payment/` | Business rules: amount > 0, `INR` only, `Idempotency-Key` required, UUIDv4 + SHA256 request hash |
| `internal/store/` | Postgres transactions (`orders.go` idempotent create, `refunds.go` idempotent atomic refund) |
| `migrations/` | Authoritative schema, applied in order (`0001_init.sql`, `0002_refund_idempotency.sql`) |
| `internal/payment/service_test.go` | Unit tests (no DB needed) |
| `internal/store/store_test.go` | DB integration tests (need `DATABASE_URL`, skip otherwise) |

## Amounts

Minor units as `int64` — paise for `INR`. Never floats. `100` = ₹1.00.

## API

### `POST /v1/orders`

Header `Idempotency-Key: <opaque client key>` (required, ≤255 chars).
Merchant is currently hardcoded to `demo-merchant` (see limitations).

```bash
curl -X POST localhost:8080/v1/orders \
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
curl localhost:8080/v1/orders/<order-uuid>
```

### `POST /v1/orders/{id}/payments`

Creates a mock-approved (`SUCCEEDED`) payment for a `CREATED` order and
flips the order to `SUCCEEDED` in the same transaction. Header
`Idempotency-Key` required — replays return `200` with the original
payment instead of charging twice. The amount must equal the order amount.

```bash
curl -X POST localhost:8080/v1/orders/<order-uuid>/payments \
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
Merchant is currently hardcoded to `demo-merchant`, like orders.

```bash
curl -X POST localhost:8080/v1/payments/<payment-uuid>/refunds \
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
curl localhost:8080/v1/payments/<payment-uuid>
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
cp .env.example .env          # set DATABASE_URL, then export it in your shell
make migrate                  # applies migrations/*.sql in order
go run ./cmd/api
```

## Test

```bash
make test                     # vet + full suite (DB tests skip without DATABASE_URL)
DATABASE_URL=... go test ./internal/store/ -v   # DB integration tests
```

Unit tests cover validation, hashing, and UUID shape. Store integration
tests cover idempotent order replay + conflict + crash-orphan adoption,
and refund replay + conflict + atomic over-refund rejection.

## Known limitations

1. No auth; single hardcoded merchant; `INR` only.
2. No graceful shutdown, health checks, rate limiting, or structured logs.
3. Payments are mock-approved synchronously — no authorize/capture split,
   no failed-payment path yet.
