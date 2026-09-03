// spike.js — a sudden order-of-magnitude burst, then back to baseline.
//
// Two things are being tested, and neither is throughput.
//
//   1. Recovery. After the spike, does latency return to the baseline profile,
//      or does the service stay wedged? A queue that drains slowly, a breaker
//      that stays open, or a cache that was evicted wholesale all show up as a
//      post-spike plateau rather than a return to baseline.
//   2. Cache stampede protection. The spike lands on a small working set with a
//      3s cache_ttl, so entries expire mid-spike. Singleflight plus the Redis
//      stampede lock are supposed to collapse the resulting concurrent misses
//      into one upstream read per key. If they do not, the burst reaches the
//      sources amplified and the source-latency histograms blow out while the
//      request rate barely moved.
//
//   k6 run test/load/spike.js
//   k6 run -e BASELINE=50 -e PEAK=1000 test/load/spike.js

import { chaos, mixedRequest, touchWorkingSet } from './common.js';

const BASELINE = Number.parseInt(__ENV.BASELINE || '50', 10);
const PEAK = Number.parseInt(__ENV.PEAK || '800', 10);

export const options = {
  scenarios: {
    spike: {
      executor: 'ramping-arrival-rate',
      startRate: BASELINE,
      timeUnit: '1s',
      preAllocatedVUs: Number.parseInt(__ENV.PRE_ALLOCATED_VUS || '100', 10),
      maxVUs: Number.parseInt(__ENV.MAX_VUS || '2000', 10),
      stages: [
        { target: BASELINE, duration: '1m' },   // settle, warm the cache
        { target: PEAK, duration: '10s' },      // the spike: near-instant
        { target: PEAK, duration: '1m' },       // hold at peak
        { target: BASELINE, duration: '10s' },  // release
        { target: BASELINE, duration: '3m' },   // recovery window — the point
      ],
      gracefulStop: '30s',
    },
  },
  thresholds: {
    // During the spike the service may shed. It may not error.
    server_errors: ['count==0'],
    envelope_valid: ['rate>0.99'],

    // Whole-run p99 is allowed 2x the healthy budget, because the spike window
    // is included in it. Recovery is asserted through p90 instead of through a
    // windowed comparison, which k6 thresholds cannot express: the 3 minute
    // recovery window is by far the largest part of the run, so if the service
    // had stayed wedged, p90 would have moved with it.
    'http_req_duration{route:status}': ['p(90)<60', 'p(99)<240'],

    // Stampede protection: a spike must not turn into a routing change. If the
    // burst had reached the sources amplified, ODS latency would rise, its
    // breaker would trip, and the operational route ratio would collapse.
    'operational_route_ratio{route:status}': ['rate>0.90'],
    'execution_fallback_ratio{route:status}': ['rate<0.05'],
    stale_serve_ratio: ['rate<0.001'],
  },
};

export function setup() {
  chaos.reset();
  touchWorkingSet();
}

export default function () {
  mixedRequest();
}

export function teardown() {
  chaos.reset();
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [
    '',
    'spike summary',
    '-------------',
    `baseline / peak         ${BASELINE} -> ${PEAK} rps`,
    `status p90 / p99        ${pct(m, 'http_req_duration{route:status}', 'p(90)')} / ${pct(m, 'http_req_duration{route:status}', 'p(99)')} ms`,
    `server-side status p99  ${pct(m, 'bff_elapsed_ms{route:status}', 'p(99)')} ms`,
    `shed (429 + 503)        ${count(m, 'shed_responses')}`,
    `500s                    ${count(m, 'server_errors')}`,
    `cache hit ratio         ${rate(m, 'cache_hit_ratio{route:status}')}`,
    `operational route ratio ${rate(m, 'operational_route_ratio{route:status}')}`,
    '',
    'How to read it:',
    '  p90 near the baseline budget with a much larger p99 is the expected',
    '  shape: the spike window is the tail. p90 also elevated means the service',
    '  did not recover, and the recovery window rather than the spike is what',
    '  you are looking at.',
    '  Check operational_source_latency on :9090/metrics across the spike. If it',
    '  rose in proportion to the request rate, stampede collapsing did not work',
    '  and the burst was amplified into the source.',
    '',
  ];
  return { stdout: lines.join('\n') };
}

function pct(metrics, name, p) {
  const v = metrics[name] && metrics[name].values[p];
  return v === undefined ? 'n/a' : Number(v).toFixed(1);
}

function rate(metrics, name) {
  const v = metrics[name] && metrics[name].values.rate;
  return v === undefined ? 'n/a' : Number(v).toFixed(4);
}

function count(metrics, name) {
  const v = metrics[name] && metrics[name].values.count;
  return v === undefined ? '0' : String(v);
}
