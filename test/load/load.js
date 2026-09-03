// load.js — steady state at nominal load.
//
// This is the profile that decides whether a build meets its SLOs
// (spec/requirements.md REQ-PERF-001) and whether the routing policy is doing
// what it claims. It holds a constant arrival rate with a realistic endpoint
// mix, weighted towards /status because that is what an operations console
// polls.
//
//   make k6-load
//   k6 run test/load/load.js
//   k6 run -e RATE=400 -e DURATION=20m test/load/load.js
//   k6 run -e TOUCH_SETUP=false test/load/load.js   # measure the seeded age spread

import { sleep } from 'k6';
import {
  AVAILABILITY_SLO,
  LATENCY_SLO,
  LOCAL_RESOURCES,
  MIX,
  TOUCH_SETUP,
  chaos,
  mixedRequest,
  touchWorkingSet,
} from './common.js';

const RATE = Number.parseInt(__ENV.RATE || '200', 10);
const DURATION = __ENV.DURATION || '10m';
const PRE_ALLOCATED = Number.parseInt(__ENV.PRE_ALLOCATED_VUS || '80', 10);
const MAX_VUS = Number.parseInt(__ENV.MAX_VUS || '400', 10);

export const options = {
  scenarios: {
    steady: {
      // Arrival rate rather than a VU count: the question is "can it serve N
      // requests per second", and an open model keeps that question honest when
      // latency rises. A closed VU model would silently reduce the offered load
      // exactly when the service starts struggling.
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: PRE_ALLOCATED,
      maxVUs: MAX_VUS,
      gracefulStop: '30s',
    },
  },
  thresholds: Object.assign(
    {},
    AVAILABILITY_SLO,
    LATENCY_SLO,
    {
      // ---- policy assertions, the ones unique to this service -------------
      //
      // With the working set touched in setup(), every operational record is
      // inside the 10s resource_status TTL, so /status must route to the
      // operational source. A drop here means either the TTL policy stopped
      // working or the operational source started failing; either way it is a
      // routing regression, not a latency one.
      'operational_route_ratio{route:status}': ['rate>0.95'],
      'ttl_hit_ratio{route:status}': ['rate>0.90'],

      // Fallback to the slow source should be rare in steady state.
      'execution_fallback_ratio{route:status}': ['rate<0.05'],

      // Degradation must not be happening at all when nothing is broken.
      stale_serve_ratio: ['rate<0.001'],
      'degraded_ratio{route:status}': ['rate<0.01'],
      'partial_ratio{route:details}': ['rate<0.02'],

      // The cache should be carrying the hot working set. cache_ttl for
      // resource_status is 3s against a working set of 13 resources, so at
      // 200 RPS the overwhelming majority of /status calls are hits.
      'cache_hit_ratio{route:status}': ['rate>0.80'],

      // Server-side time, from meta.elapsedMs. This is the measurement that
      // excludes the load generator and the network, so it is the one that can
      // be compared directly against REQ-PERF-001.
      'bff_elapsed_ms{route:status}': ['p(95)<60', 'p(99)<120'],
      'bff_elapsed_ms{route:details}': ['p(95)<280', 'p(99)<550'],
    },
  ),
};

export function setup() {
  chaos.reset();
  const touched = touchWorkingSet();
  return {
    touched,
    resources: LOCAL_RESOURCES.length,
    mix: MIX.map((m) => `${m.name}:${Math.round(m.weight * 100)}%`).join(' '),
  };
}

export default function () {
  mixedRequest();
}

export function teardown(data) {
  chaos.reset();
  console.log(
    `mix ${data.mix} over ${data.resources} resources; working set touched: ${data.touched} (TOUCH_SETUP=${TOUCH_SETUP})`,
  );
  // A short pause so the last in-flight responses are recorded before the
  // summary is computed.
  sleep(1);
}
