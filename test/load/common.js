// Shared helpers for every k6 profile in this directory.
//
// Three things live here so the six profiles stay comparable:
//
//   1. The working set. The reference operational source seeds R001..R050 and
//      assigns R00i to tenant `local` when i % 3 == 1, so a load profile that
//      picks resource ids at random would spend a third of its requests
//      collecting 404s from tenant isolation. LOCAL_RESOURCES is the exact set
//      that belongs to `local`.
//   2. Envelope accounting. Every profile records the same custom metrics from
//      meta.routingRule / meta.routingDecision / meta.freshness / meta.cache, so
//      the thresholds assert *policy* behaviour and not just transport latency.
//      This is only possible because the response publishes its own routing
//      decision.
//   3. The chaos client, so degradation.js and the setup() of the steady-state
//      profiles can drive the reference sources instead of describing them.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
export const OPS_ADMIN = __ENV.OPS_ADMIN_URL || 'http://localhost:9111';
export const EDS_ADMIN = __ENV.EDS_ADMIN_URL || 'http://localhost:9112';
export const BFF_ADMIN = __ENV.BFF_ADMIN_URL || 'http://localhost:9090';
export const TENANT = __ENV.TENANT || 'local';
export const TOKEN = __ENV.BFF_TOKEN || '';

// Set TOUCH_SETUP=false to measure the seeded age distribution instead of a
// uniformly fresh working set. See README.md, "Why setup() touches the source".
export const TOUCH_SETUP = (__ENV.TOUCH_SETUP || 'true') !== 'false';

export function apiHeaders(extra) {
  const h = { 'X-Tenant-ID': TENANT, Accept: 'application/json' };
  if (TOKEN) h.Authorization = `Bearer ${TOKEN}`;
  return Object.assign(h, extra || {});
}

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// ---------------------------------------------------------------------------
// Working set
// ---------------------------------------------------------------------------

// R00i belongs to tenant `local` when i % 3 == 1.
export const LOCAL_RESOURCES = (() => {
  const out = [];
  for (let i = 1; i <= 50; i++) {
    if (i % 3 === 1) out.push({ i, id: `R${String(i).padStart(3, '0')}` });
  }
  return out;
})();

// The operational source seeds an in-flight execution reference when i % 5 == 1.
// Those resources legitimately answer /status with routingDecision BOTH, because
// routing.defaults.resolve_in_flight_execution consults the execution source so
// /status and /details cannot disagree mid-workflow. Excluding them from the
// /status pool keeps the operational_route_ratio threshold a crisp assertion
// about TTL routing rather than a blend of two behaviours.
export const STATUS_RESOURCES = LOCAL_RESOURCES.filter((r) => r.i % 5 !== 1);
export const INFLIGHT_RESOURCES = LOCAL_RESOURCES.filter((r) => r.i % 5 === 1);

// Every seeded resource has at least execution E<id>-0 (count = (i % 4) + 1).
export function executionIdFor(r) {
  return `E${String(r.i).padStart(3, '0')}-0`;
}

export function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}

// ---------------------------------------------------------------------------
// Custom metrics — the policy assertions
// ---------------------------------------------------------------------------

export const envelopeValid = new Rate('envelope_valid');
export const ttlHitRatio = new Rate('ttl_hit_ratio');
export const operationalRouteRatio = new Rate('operational_route_ratio');
export const executionFallbackRatio = new Rate('execution_fallback_ratio');
export const staleServeRatio = new Rate('stale_serve_ratio');
export const degradedRatio = new Rate('degraded_ratio');
export const partialRatio = new Rate('partial_ratio');
export const cacheHitRatio = new Rate('cache_hit_ratio');
export const serverElapsed = new Trend('bff_elapsed_ms', true);

// Shedding versus breaking. 429 (rate limit) and 503 (no source available,
// bulkhead saturated) are the service refusing work on purpose and are expected
// under saturation or during an outage. A 500 means an internal error path was
// reached, which is a defect at any load.
//
// 206 is neither: it is a complete, correct partial answer. It is counted by
// partial_ratio, not here.
export const serverErrors = new Counter('server_errors');
export const shedResponses = new Counter('shed_responses');
// A request type with no routing rule configured. Never legitimate.
export const unconfiguredRules = new Counter('unconfigured_request_types');

// Rule ids are a frozen contract (docs/DESIGN-CONTRACT.md §5 plus the two the
// application layer stamps), which is what makes them safe to assert on.
export const RULE_FRESH = 'ttl.operational.fresh';
export const RULE_STALE = 'ttl.operational.stale';
export const RULE_UNKNOWN = 'ttl.unknown_freshness';
export const RULE_PRIMARY_DOWN = 'health.primary_unavailable';
export const RULE_BOTH_DOWN = 'health.both_unavailable';
export const RULE_CALL_FALLBACK = 'fallback.primary_failed';
export const RULE_STALE_CACHE = 'degrade.stale_cache';
export const RULE_SPAN_BOTH = 'fields.span_both';
export const RULE_OPS_ONLY = 'fields.operational_only';
export const RULE_EXEC_ONLY = 'fields.execution_only';
// Pre-chain exit: the request type has no routing rule at all. A deployment
// defect rather than a routing outcome, and it is emitted to
// routing_decision_total precisely so it cannot hide. Any occurrence fails a run.
export const RULE_UNCONFIGURED = 'guard.unconfigured_request_type';

const FALLBACK_RULES = [RULE_STALE, RULE_PRIMARY_DOWN, RULE_CALL_FALLBACK];

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// getJSON issues one request tagged with its route class, so the per-class
// latency thresholds in each profile can address it as
// http_req_duration{route:status}.
export function getJSON(path, routeTag, extraTags) {
  const params = {
    headers: apiHeaders(),
    tags: Object.assign({ route: routeTag }, extraTags || {}),
  };
  return http.get(`${BASE_URL}/api/v1${path}`, params);
}

// record parses the envelope, feeds every policy metric, and returns the parsed
// meta so a caller can make profile-specific assertions.
//
// A 5xx has no envelope; it is counted as an invalid envelope and skipped for
// the policy metrics, so an outage does not silently drag ttl_hit_ratio to zero
// through absent data rather than through a real routing change.
export function record(res, routeTag, extraTags) {
  const t = Object.assign({ route: routeTag }, extraTags || {});
  const ok = check(
    res,
    {
      // 206 is a success: a partial answer is still an answer, and it is what
      // a fallback to a source that cannot hold every requested field returns.
      'status is 2xx': (r) => r.status >= 200 && r.status < 300,
      'has json body': (r) => (r.headers['Content-Type'] || '').indexOf('application/json') === 0,
    },
    t,
  );

  if (res.status === 429 || res.status === 503) {
    shedResponses.add(1, Object.assign({ status: String(res.status) }, t));
  } else if (res.status >= 500) {
    serverErrors.add(1, Object.assign({ status: String(res.status) }, t));
  }

  let body = null;
  try {
    body = res.json();
  } catch (e) {
    body = null;
  }

  const meta = body && body.meta ? body.meta : null;
  const complete =
    ok &&
    meta !== null &&
    typeof meta.routingDecision === 'string' &&
    typeof meta.routingRule === 'string' &&
    Array.isArray(meta.sources) &&
    meta.freshness !== undefined &&
    meta.cache !== undefined;

  envelopeValid.add(complete, t);
  if (!meta) return null;

  ttlHitRatio.add(meta.routingRule === RULE_FRESH, t);
  operationalRouteRatio.add(meta.routingDecision === 'OPERATIONAL', t);
  executionFallbackRatio.add(FALLBACK_RULES.indexOf(meta.routingRule) >= 0, t);
  staleServeRatio.add(meta.routingRule === RULE_STALE_CACHE, t);
  degradedRatio.add(meta.degraded === true, t);
  partialRatio.add(meta.partial === true, t);
  cacheHitRatio.add(!!(meta.cache && meta.cache.hit), t);
  unconfiguredRules.add(meta.routingRule === RULE_UNCONFIGURED ? 1 : 0, t);
  if (typeof meta.elapsedMs === 'number') {
    serverElapsed.add(meta.elapsedMs, t);
  }
  return meta;
}

// ---------------------------------------------------------------------------
// The five request shapes, and the weighted mix
// ---------------------------------------------------------------------------

// Every shape takes optional extra tags so a profile can slice its metrics by
// something of its own — degradation.js tags every request with the chaos phase
// it was issued during, which is what makes "latency stayed bounded while the
// execution source was degraded" a threshold rather than a claim.

export function getStatus(r, tags) {
  return record(getJSON(`/resources/${r.id}/status`, 'status', tags), 'status', tags);
}

export function getRead(r, tags) {
  return record(getJSON(`/resources/${r.id}`, 'read', tags), 'read', tags);
}

export function getDetails(r, tags) {
  return record(getJSON(`/resources/${r.id}/details`, 'details', tags), 'details', tags);
}

export function getExecutions(r, tags) {
  return record(getJSON(`/resources/${r.id}/executions?limit=25`, 'executions', tags), 'executions', tags);
}

export function getExecutionStatus(r, tags) {
  return record(
    getJSON(`/resources/${r.id}/executions/${executionIdFor(r)}`, 'execution_status', tags),
    'execution_status',
    tags,
  );
}

export function getConfiguration(r, tags) {
  return record(getJSON(`/resources/${r.id}/configuration`, 'configuration', tags), 'configuration', tags);
}

// The steady-state mix. Weighted towards /status because that is what a console
// polls; the cumulative weights are what the profiles sample against.
export const MIX = [
  { weight: 0.45, name: 'status', fn: getStatus, pool: () => STATUS_RESOURCES },
  { weight: 0.25, name: 'read', fn: getRead, pool: () => LOCAL_RESOURCES },
  { weight: 0.15, name: 'details', fn: getDetails, pool: () => LOCAL_RESOURCES },
  { weight: 0.1, name: 'executions', fn: getExecutions, pool: () => LOCAL_RESOURCES },
  { weight: 0.05, name: 'execution_status', fn: getExecutionStatus, pool: () => LOCAL_RESOURCES },
];

export function mixedRequest(mix, tags) {
  const m = mix || MIX;
  const roll = Math.random();
  let acc = 0;
  for (const entry of m) {
    acc += entry.weight;
    if (roll < acc) return entry.fn(pick(entry.pool()), tags);
  }
  const last = m[m.length - 1];
  return last.fn(pick(last.pool()), tags);
}

// ---------------------------------------------------------------------------
// Chaos client — cmd/opsource :9111 and cmd/exsource :9112
// ---------------------------------------------------------------------------

export const chaos = {
  ops(body) {
    return http.put(`${OPS_ADMIN}/chaos`, JSON.stringify(body), { headers: JSON_HEADERS });
  },
  eds(body) {
    return http.put(`${EDS_ADMIN}/chaos`, JSON.stringify(body), { headers: JSON_HEADERS });
  },
  reset() {
    http.del(`${OPS_ADMIN}/chaos`);
    http.del(`${EDS_ADMIN}/chaos`);
  },
  touch(id) {
    return http.post(`${OPS_ADMIN}/resources/${id}/touch`);
  },
  age(id, seconds) {
    return http.post(`${OPS_ADMIN}/resources/${id}/age?seconds=${seconds}`);
  },
  inFlight(id, executionId) {
    return http.post(`${OPS_ADMIN}/resources/${id}/in-flight?executionId=${executionId}`);
  },
};

// touchWorkingSet resets every working-set record's age to zero so the baseline
// measures steady state rather than the seeded age spread ((i % 7) * 5 seconds,
// which puts roughly half the set outside the 10s resource_status TTL).
export function touchWorkingSet() {
  if (!TOUCH_SETUP) return false;
  for (const r of LOCAL_RESOURCES) {
    chaos.touch(r.id);
  }
  return true;
}

// ---------------------------------------------------------------------------
// Thresholds — the SLOs from spec/requirements.md REQ-PERF-001
// ---------------------------------------------------------------------------
//
// REQ-PERF-001 states server-side budgets. http_req_duration is measured at the
// load generator and therefore also contains connection reuse, TLS (none
// locally), the loopback or cluster hop and k6's own client cost. The
// source-bound classes below use the budget verbatim, because a few
// milliseconds of client cost is noise against a 60ms or 280ms budget. The
// cache-hit class is stated separately and more loosely for exactly that
// reason: the server-side 5ms/15ms budget is asserted through bff_elapsed_ms,
// which is the server's own measurement carried in meta.elapsedMs.

export const LATENCY_SLO = {
  'http_req_duration{route:status}': ['p(95)<60', 'p(99)<120'],
  'http_req_duration{route:read}': ['p(95)<60', 'p(99)<120'],
  'http_req_duration{route:configuration}': ['p(95)<60', 'p(99)<120'],
  'http_req_duration{route:executions}': ['p(95)<250', 'p(99)<500'],
  'http_req_duration{route:execution_status}': ['p(95)<250', 'p(99)<500'],
  'http_req_duration{route:details}': ['p(95)<280', 'p(99)<550'],
  'bff_elapsed_ms{route:status}': ['p(95)<60', 'p(99)<120'],
  'bff_elapsed_ms{route:details}': ['p(95)<280', 'p(99)<550'],
};

export const AVAILABILITY_SLO = {
  http_req_failed: ['rate<0.001'],
  envelope_valid: ['rate>0.999'],
  checks: ['rate>0.999'],
  // A broken routing configuration must fail the run outright, not average out.
  unconfigured_request_types: ['count==0'],
};

export function summaryLine(meta) {
  if (!meta) return 'no envelope';
  return `${meta.routingDecision} via ${meta.routingRule}` +
    ` fresh=${meta.freshness && meta.freshness.state}` +
    ` cache=${meta.cache && meta.cache.hit}` +
    ` degraded=${meta.degraded} partial=${meta.partial}`;
}
