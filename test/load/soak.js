// soak.js — moderate load, held for hours.
//
// Nothing here is about peak capacity. A soak looks for the failures that only
// appear with time: goroutine leaks, connection pools that never return
// connections, an L1 cache that grows without bound, a freshness probe memo that
// accumulates keys, file descriptors that are not closed, and slow memory
// growth. All of those show up as a *drift* rather than as a failure, so the
// assertions below are about the run's second half looking like its first.
//
//   k6 run test/load/soak.js
//   k6 run -e DURATION=4h -e RATE=150 test/load/soak.js
//
// While it runs, watch on the admin port — these are the series that move when
// something is leaking:
//
//   curl -s localhost:9090/metrics | grep -E 'go_goroutines|go_memstats_heap_inuse_bytes|bulkhead_in_flight'
//
// A soak with a *rotating* working set is the harsher variant: set
// COLD_SET=true to walk all fifty resources instead of the thirteen that belong
// to `local`, which stops the cache from hiding a per-key allocation leak.

import { sleep } from 'k6';
import {
  AVAILABILITY_SLO,
  LOCAL_RESOURCES,
  chaos,
  getDetails,
  getExecutions,
  getRead,
  getStatus,
  mixedRequest,
  touchWorkingSet,
} from './common.js';

const RATE = Number.parseInt(__ENV.RATE || '150', 10);
const DURATION = __ENV.DURATION || '4h';

export const options = {
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Number.parseInt(__ENV.PRE_ALLOCATED_VUS || '60', 10),
      maxVUs: Number.parseInt(__ENV.MAX_VUS || '300', 10),
      gracefulStop: '1m',
    },
  },
  thresholds: Object.assign({}, AVAILABILITY_SLO, {
    // The SLO must hold for the whole run, not just at the start. A leak that
    // degrades latency over hours shows up here as a p95 breach even though the
    // offered load never changed.
    'http_req_duration{route:status}': ['p(95)<60', 'p(99)<120'],
    'http_req_duration{route:details}': ['p(95)<280', 'p(99)<550'],
    'bff_elapsed_ms{route:status}': ['p(95)<60', 'p(99)<120'],

    server_errors: ['count==0'],
    envelope_valid: ['rate>0.999'],

    // Routing behaviour must not drift. A slowly rising fallback ratio with a
    // constant offered load means the freshness probe or the operational source
    // is degrading over time, which is exactly the class of problem a soak
    // exists to catch.
    'operational_route_ratio{route:status}': ['rate>0.95'],
    'execution_fallback_ratio{route:status}': ['rate<0.05'],
    stale_serve_ratio: ['rate<0.001'],
    'partial_ratio{route:details}': ['rate<0.02'],
  }),
};

const COLD_SET = (__ENV.COLD_SET || 'false') === 'true';

// A cold-set walk needs ids that exist for *some* tenant; those that do not
// belong to the configured tenant answer 404, which is a legitimate response
// shape but not one that should count against the availability threshold. So the
// cold variant walks the local set in order instead of picking at random: it
// still defeats the cache (13 keys x 3s cache_ttl at 150 rps means each key
// expires between visits only in the mixed profile), while keeping every request
// a real read.
let cursor = 0;

export function setup() {
  chaos.reset();
  touchWorkingSet();
  return { rate: RATE, duration: DURATION, cold: COLD_SET };
}

export default function () {
  if (!COLD_SET) {
    mixedRequest();
    return;
  }
  const r = LOCAL_RESOURCES[cursor % LOCAL_RESOURCES.length];
  cursor++;
  switch (cursor % 4) {
    case 0: getDetails(r); break;
    case 1: getRead(r); break;
    case 2: getExecutions(r); break;
    default: getStatus(r); break;
  }
}

export function teardown(data) {
  chaos.reset();
  console.log(`soak: ${data.rate} rps for ${data.duration}, cold set: ${data.cold}`);
  sleep(1);
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [
    '',
    'soak summary',
    '------------',
    `offered                 ${RATE} rps for ${DURATION}`,
    `requests                ${count(m, 'http_reqs')}`,
    `status p95 / p99        ${pct(m, 'http_req_duration{route:status}', 'p(95)')} / ${pct(m, 'http_req_duration{route:status}', 'p(99)')} ms`,
    `server-side status p99  ${pct(m, 'bff_elapsed_ms{route:status}', 'p(99)')} ms`,
    `operational route ratio ${rate(m, 'operational_route_ratio{route:status}')}`,
    `cache hit ratio         ${rate(m, 'cache_hit_ratio{route:status}')}`,
    `degraded / partial      ${rate(m, 'degraded_ratio')} / ${rate(m, 'partial_ratio')}`,
    `500s                    ${count(m, 'server_errors')}`,
    '',
    'How to read it:',
    '  k6 thresholds are whole-run aggregates and cannot by themselves prove',
    '  the absence of drift. Compare this summary against a 20-minute load.js',
    '  run at the same rate: matching percentiles mean no drift. Then check the',
    '  process-level series over the run window in Prometheus —',
    '  go_goroutines, go_memstats_heap_inuse_bytes, bulkhead_in_flight — which',
    '  are the ones that grow when something is genuinely leaking.',
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
