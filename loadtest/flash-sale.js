// Load test for the FIXED checkout.
// Many virtual users hit /api/checkout at the same time, like the 8 PM rush.
// Run (PowerShell, from the project folder):
//   docker run --rm --network flashsale-net -v "${PWD}/loadtest:/scripts" grafana/k6 run /scripts/flash-sale.js
// Bigger burst:  add  -e VUS=2000 -e REQUESTS=50000  after "run"
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://nginx';
const HEADERS = {
  'Content-Type': 'application/json',
  'X-Loadtest-Key': __ENV.LOADTEST_KEY || 'local-loadtest-only', // skip per-IP limit: all test traffic is from one IP
};

const accepted = new Counter('accepted_202');
const soldOut = new Counter('sold_out_409');
const duplicates = new Counter('double_click_same_order');
const unexpected = new Counter('unexpected_status');

export const options = {
  scenarios: {
    rush: {
      executor: 'shared-iterations',
      vus: Number(__ENV.VUS || 1000),           // users clicking at the same moment
      iterations: Number(__ENV.REQUESTS || 20000),
      maxDuration: '3m',
    },
  },
  // 202 and 409 are both correct answers. Anything else counts as "dropped".
  thresholds: {
    http_req_failed: ['rate<0.001'],            // target: 0% dropped requests
    http_req_duration: ['p(95)<500'],
  },
};
http.setResponseCallback(http.expectedStatuses(202, 409));

export function setup() {
  if ((__ENV.RESET || '1') === '1') {
    const r = http.post(`${BASE}/api/admin/reset?delay=0`, null, {
      headers: { 'X-Admin-Token': __ENV.ADMIN || 'demo-admin-token' },
    });
    check(r, { 'reset ok': (x) => x.status === 200 });
    sleep(1);
  }
}

export default function () {
  const user = `u${__VU}_${__ITER}`;
  const body = JSON.stringify({ userId: user });
  const r = http.post(`${BASE}/api/checkout`, body, { headers: HEADERS });

  if (r.status === 202) accepted.add(1);
  else if (r.status === 409) soldOut.add(1);
  else unexpected.add(1);
  check(r, { '202 or 409': (x) => x.status === 202 || x.status === 409 });

  // 5% of users "double click": the second request must return the SAME order.
  if (r.status === 202 && Math.random() < 0.05) {
    const again = http.post(`${BASE}/api/checkout`, body, { headers: HEADERS });
    const same = again.status === 202 && again.json('orderId') === r.json('orderId');
    if (same) duplicates.add(1);
    check(again, { 'double click -> same order': () => same });
  }
}

export function teardown() {
  sleep(5); // give the workers a moment to drain the queue
  const s = http.get(`${BASE}/api/stats`, { responseCallback: http.expectedStatuses(200) }).json();
  console.log('================ RESULT ================');
  console.log(`Stock:              ${s.totalStock}`);
  console.log(`Confirmed in DB:    ${s.confirmedOrders}`);
  console.log(`DB stock left:      ${s.dbStockLeft}`);
  console.log(`Redis tokens left:  ${s.redisStockLeft}`);
  console.log(`Still in queue:     ${s.queuePending}`);
  console.log(`OVERSOLD?           ${s.oversold ? 'YES  <-- bug' : 'NO  (exactly ' + s.confirmedOrders + ')'}`);
  console.log('========================================');
}
