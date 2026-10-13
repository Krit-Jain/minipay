# MiniPay

Minimal payments backend in Go (tiny Stripe/Razorpay clone).
Stdlib `net/http` + Postgres via `pgxpool`. No web framework.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/api/main.go` | Wiring: DB pool → repo → service → handlers → `ServeMux` on `:8080` |
| `internal/httpapi/` | HTTP validation, JSON parsing, status-code mapping (`orders.go`, `refunds.go`) |
| `internal/payment/` | Business rules: amount > 0, `INR` only, `Idempotency-Key` required, UUIDv4 + SHA256 request hash |
| `internal/store/` | Postgres transactions (`orders.go` idempotent create, `refunds.go` atomic refund) |
| `migrations/0001_init.sql` | Authoritative schema |
| `internal/payment/service_test.go` | Unit tests (no DB needed) |

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

How idempotency works (`store/orders.go`): `INSERT ... ON CONFLICT DO NOTHING`
into `idempotency_keys(merchant_id, key, request_hash)`, then
`SELECT ... FOR UPDATE`, compare hashes, return existing order or insert
order + link key in the same transaction.

### `POST /v1/payments/{id}/refunds`

```bash
curl -X POST localhost:8080/v1/payments/<payment-uuid>/refunds \
  -H 'Content-Type: application/json' \
  -d '{"amount":10000}'
```

Responses: `201` created (`PENDING`), `400` bad amount/ID, `409` payment
missing / not `SUCCEEDED` / insufficient refundable balance, `500` otherwise.

Anti-over-refund (`store/refunds.go`): single conditional
`UPDATE payments SET refunded_amount += $1 WHERE status='SUCCEEDED' AND ...`
— losers get zero rows → `409`. Refund row is inserted in the same tx.

## Run

```bash
cp .env.example .env          # set DATABASE_URL
psql "$DATABASE_URL" -f migrations/0001_init.sql
go run ./cmd/api
```

## Test

```bash
go vet ./...
go test ./...
```

DB integration tests (concurrent idempotency / refund races) are planned;
currently only pure-logic unit tests run without a database.

## Known limitations (being fixed step by step)

1. Refunds lack `Idempotency-Key` — retry can double-refund.
2. Invalid payment UUID surfaces as `500` instead of `400`.
3. Over-long idempotency key surfaces as `500` instead of `400`.
4. Crashed key-claim (key with `order_id NULL`) wedges that key.
5. No `payments` creation API — orders and payments are disconnected.
6. No auth; single hardcoded merchant; `INR` only.
7. No graceful shutdown, health checks, rate limiting, or structured logs.
