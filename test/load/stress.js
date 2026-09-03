// stress.js — find the knee.
//
// A staircase of arrival rates, run until the service stops meeting its SLO.
// The output that matters is not pass/fail; it is the stage at which p99 leaves
// the budget and *how* it leaves it. This service is meant to shed load rather
// than queue it: at saturation the per-source bulkheads and the rate limiter
// should produce bounded latency plus 429/503, never unbounded latency and never
// 500s (REQ-PERF-005).
//
//   k6 run test/load/stress.js
//   k6 run -e START_RATE=200 -e STEP=200 -e STAGES=8 -e STAGE_DURATION=2m test/load/stress.js
//
// For the per-stage breakdown rather than the whole-run aggregate:
//   k6 run --out json=stress.json test/load/stress.js

import { chaos, mixedRequest, touchWorkingSet } from './common.js';

const START_RATE = Number.parseInt(__ENV.START_RATE || '100', 10);
const STEP = Number.parseInt(__ENV.STEP || '150', 10);
const STAGES = Number.parseInt(__ENV.STAGES || '8', 10);
const STAGE_DURATION = __ENV.STAGE_DURATION || '2m';
const TOP_RATE = START_RATE + (STAGES - 1) * STEP;

// Each stage ramps for 30s and then holds, so every measurement is taken at a
// steady offered rate rather than during a transient.
const stages = [];
for (let i = 0; i < STAGES; i++) {
  const target = START_RATE + i * STEP;
  stages.push({ target, duration: '30s' });
  stages.push({ target, duration: STAGE_DURATION });
}

export const options = {
  scenarios: {
    staircase: {
      executor: 'ramping-arrival-rate',
      startRate: START_RATE,
      timeUnit: '1s',
      preAllocatedVUs: Number.parseInt(__ENV.PRE_ALLOCATED_VUS || '200', 10),
      maxVUs: Number.parseInt(__ENV.MAX_VUS || '2000', 10),
      stages,
      gracefulStop: '30s',
    },
  },
  // Deliberately looser than load.js: the point of this profile is to run past
  // the SLO. What must hold all the way to the top of the staircase is the
  // *shape* of the failure, not the latency itself.
  thresholds: {
    // Shedding is graceful. A 500 means an internal error path was reached,
    // which is a defect regardless of load.
    server_errors: ['count==0'],

    // Bounded degradation: p99 may leave the 120ms status budget, but not by
    // more than 2x the healthy budget over the whole run (REQ-PERF-005).
    'http_req_duration{route:status}': ['p(99)<240'],
    'http_req_duration{route:details}': ['p(99)<1100'],

    // Refusals must stay a minority even at the top of the staircase.
    http_req_failed: ['rate<0.30'],

    // The envelope must stay intact under pressure: a truncated or malformed
    // response is a correctness failure, not a capacity one.
    envelope_valid: ['rate>0.99'],

    // Routing must not silently change shape under load. If the operational
    // route collapses here, the ODS breaker or bulkhead is being tripped by
    // BFF-side queuing rather than by the source actually being unwell — which
    // is a different bug from "we ran out of capacity".
    'operational_route_ratio{route:status}': ['rate>0.80'],
  },
};

export function setup() {
  chaos.reset();
  touchWorkingSet();
  return { top: TOP_RATE };
}

export default function () {
  mixedRequest();
}

export function teardown() {
  chaos.reset();
}

// handleSummary states the result in words. A wall of percentiles does not tell
// you where the knee was.
export function handleSummary(data) {
  const m = data.metrics;
  const lines = [
    '',
    'stress summary',
    '--------------',
    `top offered rate        ${TOP_RATE} rps over ${STAGES} stages of ${STAGE_DURATION}`,
    `status p95 / p99        ${pct(m, 'http_req_duration{route:status}', 'p(95)')} / ${pct(m, 'http_req_duration{route:status}', 'p(99)')} ms   (budget 60 / 120)`,
    `details p95 / p99       ${pct(m, 'http_req_duration{route:details}', 'p(95)')} / ${pct(m, 'http_req_duration{route:details}', 'p(99)')} ms   (budget 280 / 550)`,
    `server-side status p99  ${pct(m, 'bff_elapsed_ms{route:status}', 'p(99)')} ms`,
    `request failure rate    ${rate(m, 'http_req_failed')}`,
    `shed (429 + 503)        ${count(m, 'shed_responses')}`,
    `500s                    ${count(m, 'server_errors')}`,
    `operational route ratio ${rate(m, 'operational_route_ratio{route:status}')}`,
    `cache hit ratio         ${rate(m, 'cache_hit_ratio{route:status}')}`,
    '',
    'How to read it:',
    '  The knee is the first stage where status p99 crosses 120 ms while the',
    '  failure rate is still near zero. Past the knee, shedding should hold p99',
    '  roughly flat while 429/503 rises. A p99 that keeps climbing *without*',
    '  shed responses rising means work is queuing somewhere unbounded — check',
    '  bulkhead_in_flight and bulkhead_rejected_total on :9090/metrics.',
    '  A falling operational_route_ratio means the load test tripped a source',
    '  breaker, so the numbers past that point describe a degraded system, not',
    '  a saturated one.',
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
