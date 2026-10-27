// Load test for the NAIVE (original, broken) checkout: SELECT stock, then UPDATE.
// Expect MORE than 500 orders: this reproduces the "540 sold out of 500" bug.
// Run (PowerShell, from the project folder):
//   docker run --rm --network flashsale-net -v "${PWD}/loadtest:/scripts" grafana/k6 run /scripts/naive.js
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE = __ENV.BASE || 'http://nginx';
const HEADERS = { 'Content-Type': 'application/json', 'X-Loadtest-Key': __ENV.LOADTEST_KEY || 'local-loadtest-only' };

export const options = {
  scenarios: {
    rush: {
      executor: 'shared-iterations',
      vus: Number(__ENV.VUS || 300),
      iterations: Number(__ENV.REQUESTS || 3000),
      maxDuration: '3m',
    },
  },
};
http.setResponseCallback(http.expectedStatuses(200, 409));

export function setup() {
  if ((__ENV.RESET || '1') === '1') {
    http.post(`${BASE}/api/admin/reset?delay=0`, null, {
      headers: { 'X-Admin-Token': __ENV.ADMIN || 'demo-admin-token' },
    });
    sleep(1);
  }
}

export default function () {
  const r = http.post(`${BASE}/api/naive/checkout`, JSON.stringify({ userId: `n${__VU}_${__ITER}` }), { headers: HEADERS });
  check(r, { '200 or 409': (x) => x.status === 200 || x.status === 409 });
}

export function teardown() {
  const s = http.get(`${BASE}/api/stats`, { responseCallback: http.expectedStatuses(200) }).json();
  console.log('================ NAIVE RESULT ================');
  console.log(`Stock:            ${s.totalStock}`);
  console.log(`Orders accepted:  ${s.naive.orders}`);
  console.log(`OVERSOLD BY:      ${s.naive.oversoldBy}`);
  console.log('==============================================');
}
