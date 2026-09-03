// degradation.js — prove that latency stays bounded while a source degrades.
//
// Every other profile measures a healthy system. This one breaks the sources
// mid-run, on a schedule, and asserts the *shape* of the degradation:
//
//   - When the execution source is slow, /status is unaffected, because a fresh
//     operational record means the execution source is never contacted at all.
//   - When the execution source is slow, /details is bounded by
//     per_source_timeout.execution (1500ms), not by how slow the source is. The
//     execution branch is optional, so the answer arrives as a 206 with
//     partial: true rather than as an error or as a 2.5 second wait.
//   - When the operational record goes stale, /status routes to the execution
//     source and picks up its latency profile — the trade the design makes on
//     purpose — without any errors.
//   - When both sources are down, requests are answered from stale cache or
//     refused cleanly. Never a 500, never an unbounded wait.
//   - When the chaos is removed, routing returns to the operational path.
//
// Two scenarios run concurrently: `traffic` generates load for the whole run,
// and `controller` is a single VU that flips the chaos knobs on a timer. Every
// request is tagged with the phase it was issued during, which is what turns
// each of the claims above into a threshold.
//
//   k6 run test/load/degradation.js
//   k6 run -e PHASE_SECONDS=90 -e RATE=150 test/load/degradation.js
//
// It requires the reference sources' admin ports to be reachable
// (OPS_ADMIN_URL, EDS_ADMIN_URL), so it runs against the compose stack or a
// non-production environment — not against a real deployment.

import { sleep } from 'k6';
import {
  LOCAL_RESOURCES,
  STATUS_RESOURCES,
  chaos,
  getDetails,
  getExecutions,
  getStatus,
  pick,
  touchWorkingSet,
} from './common.js';

const RATE = Number.parseInt(__ENV.RATE || '120', 10);
const PHASE_SECONDS = Number.parseInt(__ENV.PHASE_SECONDS || '120', 10);

// The phase schedule. Order matters: each phase is entered from a known state,
// and the controller restores that state before moving on.
const PHASES = [
  'baseline',      // everything healthy — the control
  'eds_slow',      // execution source at 2.5s, past its 1.5s per-source budget
  'eds_down',      // execution source refusing every call
  'ods_stale',     // operational records aged 60s past the 10s status TTL
  'both_down',     // total outage — the stale-cache rung
  'recover',       // chaos removed
];

const TOTAL_SECONDS = PHASES.length * PHASE_SECONDS;

export const options = {
  scenarios: {
    traffic: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: `${TOTAL_SECONDS}s`,
      preAllocatedVUs: Number.parseInt(__ENV.PRE_ALLOCATED_VUS || '80', 10),
      maxVUs: Number.parseInt(__ENV.MAX_VUS || '600', 10),
      exec: 'traffic',
      gracefulStop: '20s',
    },
    controller: {
      executor: 'per-vu-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: `${TOTAL_SECONDS + 60}s`,
      exec: 'controller',
    },
  },
  thresholds: {
    // ---- the control -------------------------------------------------------
    'http_req_duration{phase:baseline,route:status}': ['p(95)<60', 'p(99)<120'],
    'http_req_duration{phase:baseline,route:details}': ['p(95)<280', 'p(99)<550'],
    'operational_route_ratio{phase:baseline,route:status}': ['rate>0.95'],

    // ---- execution source slow --------------------------------------------
    // /status must not move at all: a fresh operational record means the
    // execution source is not on its path.
    'http_req_duration{phase:eds_slow,route:status}': ['p(95)<60', 'p(99)<120'],
    'operational_route_ratio{phase:eds_slow,route:status}': ['rate>0.95'],

    // /details must be bounded by the per-source timeout (1500ms) plus the
    // operational branch and overhead — NOT by the source's 2500ms latency.
    // This threshold is the whole point of the profile.
    'http_req_duration{phase:eds_slow,route:details}': ['p(99)<2000'],
    // ...and the bound is achieved by degrading, not by failing.
    'partial_ratio{phase:eds_slow,route:details}': ['rate>0.80'],

    // ---- execution source down --------------------------------------------
    'http_req_duration{phase:eds_down,route:status}': ['p(95)<60', 'p(99)<120'],
    'http_req_duration{phase:eds_down,route:details}': ['p(95)<280', 'p(99)<550'],
    'partial_ratio{phase:eds_down,route:details}': ['rate>0.90'],

    // ---- operational data stale -------------------------------------------
    // /status now routes to the execution source. That is a deliberate trade:
    // correctness costs latency, and the budget becomes the EXECUTION class
    // budget rather than the OPERATIONAL one.
    'execution_fallback_ratio{phase:ods_stale,route:status}': ['rate>0.50'],
    'http_req_duration{phase:ods_stale,route:status}': ['p(95)<250', 'p(99)<500'],

    // ---- total outage ------------------------------------------------------
    // Answers come from stale cache, or are refused cleanly. What must not
    // happen is an unbounded wait or an internal error.
    'http_req_duration{phase:both_down,route:status}': ['p(99)<1000'],

    // ---- recovery ----------------------------------------------------------
    'operational_route_ratio{phase:recover,route:status}': ['rate>0.90'],
    'http_req_duration{phase:recover,route:status}': ['p(95)<60', 'p(99)<120'],

    // ---- invariants across every phase ------------------------------------
    // Degradation is never an internal error, and the envelope is always
    // parseable — including on the degraded and partial paths, which are the
    // ones most likely to be built wrong.
    server_errors: ['count==0'],
    envelope_valid: ['rate>0.98'],
  },
};

export function setup() {
  chaos.reset();
  touchWorkingSet();
  // Prime the cache so the both_down phase has something to serve from. The
  // entry's physical lifetime is cache_ttl + cache.stale_grace (3s + 5m), which
  // comfortably covers the run.
  for (const r of LOCAL_RESOURCES) {
    getStatus(r, { phase: 'warmup' });
  }
  return { startMs: Date.now() };
}

// phaseFor derives the current phase from wall-clock elapsed time. Each VU runs
// its own module instance, so a shared variable would not propagate; the clock
// is the only thing every VU and the controller agree on.
function phaseFor(startMs) {
  const elapsed = (Date.now() - startMs) / 1000;
  const idx = Math.min(PHASES.length - 1, Math.floor(elapsed / PHASE_SECONDS));
  return PHASES[idx];
}

export function traffic(data) {
  const phase = phaseFor(data.startMs);
  const tags = { phase };

  // A /status-heavy mix, with enough /details to make the fan-out thresholds
  // meaningful and a little history traffic to keep the execution-only path
  // exercised.
  const roll = Math.random();
  if (roll < 0.6) {
    getStatus(pick(STATUS_RESOURCES), tags);
  } else if (roll < 0.9) {
    getDetails(pick(LOCAL_RESOURCES), tags);
  } else {
    getExecutions(pick(LOCAL_RESOURCES), tags);
  }
}

export function controller(data) {
  // Phase 1: baseline. Nothing to do but let the traffic scenario measure a
  // healthy system.
  hold(data, 'baseline');

  // Phase 2: the execution source becomes slower than its per-source budget.
  // 2500ms against per_source_timeout.execution = 1500ms, with no jitter so the
  // effect is unambiguous.
  console.log('[controller] eds_slow: base_latency_ms=2500');
  chaos.eds({ base_latency_ms: 2500, jitter_ms: 0 });
  hold(data, 'eds_slow');

  // Phase 3: the execution source refuses everything.
  console.log('[controller] eds_down: unavailable=true');
  chaos.eds({ base_latency_ms: 120, jitter_ms: 60, unavailable: true });
  hold(data, 'eds_down');

  // Phase 4: the execution source recovers, but every operational record is
  // aged 60s — well past the 10s resource_status TTL and the 30s resource_read
  // and resource_details TTLs.
  console.log('[controller] ods_stale: stale_by_seconds=60, eds restored');
  chaos.eds({ unavailable: false, base_latency_ms: 120, jitter_ms: 60 });
  chaos.ops({ stale_by_seconds: 60 });
  hold(data, 'ods_stale');

  // Phase 5: total outage. The cache primed in setup() is the only thing left.
  console.log('[controller] both_down: both sources unavailable');
  chaos.ops({ unavailable: true });
  chaos.eds({ unavailable: true });
  hold(data, 'both_down');

  // Phase 6: everything back.
  console.log('[controller] recover: chaos reset');
  chaos.reset();
  touchWorkingSet();
  hold(data, 'recover');
}

// hold sleeps until the named phase is over, using the same clock the traffic
// scenario reads, so the controller cannot drift out of step with the tags.
function hold(data, phase) {
  const end = data.startMs + (PHASES.indexOf(phase) + 1) * PHASE_SECONDS * 1000;
  while (Date.now() < end) {
    sleep(1);
  }
}

export function teardown() {
  chaos.reset();
  touchWorkingSet();
}

export function handleSummary(data) {
  const m = data.metrics;
  const rows = PHASES.map((p) => [
    p.padEnd(11),
    pct(m, `http_req_duration{phase:${p},route:status}`, 'p(95)').padStart(8),
    pct(m, `http_req_duration{phase:${p},route:status}`, 'p(99)').padStart(8),
    pct(m, `http_req_duration{phase:${p},route:details}`, 'p(99)').padStart(9),
    rate(m, `operational_route_ratio{phase:${p},route:status}`).padStart(9),
    rate(m, `execution_fallback_ratio{phase:${p},route:status}`).padStart(9),
    rate(m, `partial_ratio{phase:${p},route:details}`).padStart(8),
    rate(m, `stale_serve_ratio{phase:${p}}`).padStart(8),
  ].join(' '));

  const lines = [
    '',
    'degradation summary — one row per chaos phase',
    '',
    '            status   status  details      ops%  fallback%  partial%   stale%',
    '               p95      p99      p99                                        ',
    '-------------------------------------------------------------------------------',
    ...rows,
    '-------------------------------------------------------------------------------',
    `500s: ${count(m, 'server_errors')}   shed (429+503): ${count(m, 'shed_responses')}   envelope valid: ${rate(m, 'envelope_valid')}`,
    '',
    'How to read it:',
    '  eds_slow / eds_down: the status p95 and p99 columns must look like the',
    '  baseline row. If they moved, the execution source is on the /status path',
    '  when it should not be — most likely the operational records aged out, so',
    '  check the ops% column for that row too.',
    '  eds_slow details p99 must sit near 1500-2000 ms, not near 2500 ms: the',
    '  per-source timeout is what bounds it, and partial% near 1.0 is the proof',
    '  that the bound was achieved by degrading rather than by failing.',
    '  ods_stale: ops% collapses and fallback% rises. Latency moves to the',
    '  execution class. This is the design working, not the design failing.',
    '  both_down: stale% is how much traffic the cache absorbed. The rest is',
    '  503 NO_SOURCE_AVAILABLE, which is a correct answer, not an error.',
    '  recover: every column should return to the baseline row. If ops% stays',
    '  low, a source breaker is still open — it needs open_timeout (5s) plus two',
    '  successful probes to close.',
    '',
  ];
  return { stdout: lines.join('\n') };
}

function pct(metrics, name, p) {
  const v = metrics[name] && metrics[name].values[p];
  return v === undefined ? 'n/a' : Number(v).toFixed(0);
}

function rate(metrics, name) {
  const v = metrics[name] && metrics[name].values.rate;
  return v === undefined ? 'n/a' : Number(v).toFixed(3);
}

function count(metrics, name) {
  const v = metrics[name] && metrics[name].values.count;
  return v === undefined ? '0' : String(v);
}
