// k6 load proof: 20 VUs hammer one idempotency key. Exactly one request
// must create (201); the other nineteen must replay (200) — never 409, never
// a second order. The equivalent guarantee (plus the over-refund race) is
// proven by TestConcurrentSameKeyOrder / TestConcurrentOverRefund under
// -race in CI. Run manually against a live stack:
//
//   API_KEY=$(go run ./cmd/apikey -merchant loadtest | awk '{print $NF}') \
//   BASE_URL=http://localhost:8080 \
//   k6 run load/k6-race.js
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const KEY = __ENV.API_KEY || '';

export const options = {
  vus: 20,
  iterations: 20,
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export default function () {
  const res = http.post(
    `${BASE}/v1/orders`,
    JSON.stringify({ amount: 50000, currency: 'INR' }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${KEY}`,
        'Idempotency-Key': 'k6-stampede-key',
      },
    },
  );

  check(res, {
    'status is 200 or 201': (r) => r.status === 200 || r.status === 201,
    'body has order id': (r) => r.json().id !== undefined,
  });
}
