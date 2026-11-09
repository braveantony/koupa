// A second load tool for plan A14 「另一套工具」 (design 5.6): the same
// customer as Rush Hour (rush-hour/load/customer.go) written for k6.
//
//   constant-arrival-rate RATE/s (default 20) for DURATION (default 60s)
//   each order: 1..4 cups over menu items 1..4, client_ref <run>-c<n>
//   per-attempt timeout TIMEOUT (default 5s), retried RETRIES times
//   (default 1) at once with the same client_ref and body unless the
//   attempt got a complete 2xx response (Rush Hour's shouldRetry)
//   X-Koupa-Attempt <run>-a<n>-t<try> (internal/wire.NewAttemptID)
//
// Counted per order, as on the scorecard: 第一次送出沒有成功回應
// (koupa_first_try_failed) and 重試後仍沒有成功回應 (koupa_failed_after_retry).
// Each attempt is also printed as "ATTEMPT {json}" on the console (stderr)
// for the evidence; setup() prints "SENDING_STARTED <unix-ms>" just before
// the scenario starts. The summary goes to OUT_DIR/summary.json.
//
// Run by compare-tools.sh in the pinned grafana/k6 container.
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

const BASE = (__ENV.BASE_URL || '').replace(/\/+$/, '');
const RUN = __ENV.RUN_ID || 'k6';
const RATE = Number(__ENV.RATE || 20);
const DURATION = __ENV.DURATION || '60s';
const TIMEOUT = __ENV.TIMEOUT || '5s';
const RETRIES = Number(__ENV.RETRIES || 1);
const OUT_DIR = __ENV.OUT_DIR || '/out';

export const options = {
  scenarios: {
    orders: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      // 20/s x (5 s timeout x 2 tries) = 200 in flight at worst.
      preAllocatedVUs: 250,
      maxVUs: 600,
      gracefulStop: '30s',
    },
  },
  // Rush Hour does not set one either; k6's default would be 60 s.
  setupTimeout: '30s',
  summaryTrendStats: ['min', 'med', 'p(99)', 'max'],
};

const orders = new Counter('koupa_orders');
const firstTryFailed = new Counter('koupa_first_try_failed');
const failedAfterRetry = new Counter('koupa_failed_after_retry');
const attempts = new Counter('koupa_attempts');

function randomItems() {
  const total = 1 + Math.floor(Math.random() * 4);
  const qty = new Map();
  for (let i = 0; i < total; i++) {
    const id = 1 + Math.floor(Math.random() * 4);
    qty.set(id, (qty.get(id) || 0) + 1);
  }
  return [...qty].map(([id, q]) => ({ menu_item_id: id, quantity: q }));
}

export function setup() {
  if (!BASE) throw new Error('BASE_URL is required');
  console.log('SENDING_STARTED ' + Date.now());
}

export default function () {
  const seq = exec.scenario.iterationInTest + 1;
  const ref = `${RUN}-c${seq}`;
  const body = JSON.stringify({ client_ref: ref, items: randomItems() });
  let firstOK = false;
  let finalOK = false;
  for (let t = 1; t <= 1 + RETRIES; t++) {
    const attempt = `${RUN}-a${seq}-t${t}`;
    const sent = Date.now();
    const r = http.post(`${BASE}/orders`, body, {
      headers: { 'Content-Type': 'application/json', 'X-Koupa-Attempt': attempt },
      timeout: TIMEOUT,
      tags: { name: 'POST /orders', try: String(t) },
    });
    // Complete 2xx: a status and no transport error (k6 reads the body).
    const good = r.status >= 200 && r.status < 300 && !r.error_code;
    attempts.add(1, { try: String(t), outcome: good ? 'ok' : r.status ? 'status_' + r.status : 'error_' + r.error_code });
    console.log('ATTEMPT ' + JSON.stringify({
      client_ref: ref, attempt, try: t, sent_at: sent, ms: Date.now() - sent, status: r.status,
      error_code: r.error_code, error: r.error, pod_uid: r.headers['X-Koupa-Pod-Uid'] || null,
    }));
    if (good) {
      if (t === 1) firstOK = true;
      finalOK = true;
      break;
    }
  }
  orders.add(1);
  if (!firstOK) firstTryFailed.add(1);
  if (!finalOK) failedAfterRetry.add(1);
}

export function handleSummary(data) {
  const v = (m) => (data.metrics[m] ? data.metrics[m].values.count : 0);
  const short = {
    run: RUN, rate: RATE, duration: DURATION, timeout: TIMEOUT, retries: RETRIES,
    orders: v('koupa_orders'), first_try_failed: v('koupa_first_try_failed'),
    failed_after_retry: v('koupa_failed_after_retry'), attempts: v('koupa_attempts'),
    dropped_iterations: v('dropped_iterations'),
  };
  return {
    [`${OUT_DIR}/summary.json`]: JSON.stringify(data, null, 1),
    [`${OUT_DIR}/result.json`]: JSON.stringify(short, null, 1),
    stdout: JSON.stringify(short) + '\n',
  };
}
