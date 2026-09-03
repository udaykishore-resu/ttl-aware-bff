// smoke.js — does the deployment work at all?
//
// One virtual user, one pass over every endpoint plus the admin surface. This is
// the profile CI runs after a deploy: it is not a performance measurement, it is
// a statement that the routing policy is wired up and the envelope is intact. If
// smoke fails, no other profile's result means anything.
//
//   k6 run test/load/smoke.js
//   k6 run -e BASE_URL=https://bff.dev.example.com test/load/smoke.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import {
  BASE_URL,
  BFF_ADMIN,
  LOCAL_RESOURCES,
  STATUS_RESOURCES,
  INFLIGHT_RESOURCES,
  chaos,
  executionIdFor,
  getJSON,
  record,
  summaryLine,
  RULE_FRESH,
  RULE_SPAN_BOTH,
  RULE_OPS_ONLY,
  RULE_EXEC_ONLY,
  TOUCH_SETUP,
} from './common.js';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: {
    // A smoke run has no tolerance: one bad response is a failed deploy.
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
    envelope_valid: ['rate==1'],
    // Nothing here is under load, so the SLO is the ceiling, not the target.
    'http_req_duration{route:status}': ['p(100)<500'],
    'http_req_duration{route:details}': ['p(100)<2000'],
  },
};

export function setup() {
  // Make the freshness verdict deterministic for the one resource whose routing
  // rule this profile asserts. Without it the seeded age of that record decides
  // the rule, and the assertion becomes a coin toss on which resource was picked.
  const target = STATUS_RESOURCES[0];
  if (TOUCH_SETUP) chaos.touch(target.id);
  return { fresh: target };
}

export default function (data) {
  // --- admin surface --------------------------------------------------------
  const healthz = http.get(`${BFF_ADMIN}/healthz`, { tags: { route: 'admin' } });
  check(healthz, { 'healthz 200': (r) => r.status === 200 });

  const readyz = http.get(`${BFF_ADMIN}/readyz`, { tags: { route: 'admin' } });
  check(readyz, {
    'readyz 200': (r) => r.status === 200,
    'readyz reports both sources': (r) => {
      const b = r.json();
      return b && b.sources && b.sources.operational && b.sources.execution;
    },
  });

  const routing = http.get(`${BFF_ADMIN}/config/routing`, { tags: { route: 'admin' } });
  check(routing, {
    'config/routing 200': (r) => r.status === 200,
    'resource_status ttl is configured': (r) => {
      const b = r.json();
      return b && b.request_types && b.request_types.resource_status &&
        typeof b.request_types.resource_status.ttl === 'string';
    },
    'cache_ttl is not larger than ttl': (r) => {
      const rt = r.json().request_types.resource_status;
      // Both are Go duration strings; compare the parsed seconds.
      return seconds(rt.cache_ttl) <= seconds(rt.ttl);
    },
  });

  const metrics = http.get(`${BFF_ADMIN}/metrics`, { tags: { route: 'admin' } });
  check(metrics, {
    'metrics 200': (r) => r.status === 200,
    'routing_decision_total is exported': (r) => r.body.indexOf('routing_decision_total') >= 0,
    'operational_ttl_hit_total is exported': (r) => r.body.indexOf('operational_ttl_hit_total') >= 0,
  });

  // --- /status on a freshly touched record ---------------------------------
  const fresh = data.fresh;
  const statusMeta = record(getJSON(`/resources/${fresh.id}/status`, 'status'), 'status');
  check(statusMeta, {
    '/status routes to the operational source': (m) => m.routingDecision === 'OPERATIONAL',
    '/status fires the TTL-fresh rule': (m) => !TOUCH_SETUP || m.routingRule === RULE_FRESH,
    '/status reports FRESH': (m) => !TOUCH_SETUP || m.freshness.state === 'FRESH',
    '/status is not degraded': (m) => m.degraded === false,
    '/status is not partial': (m) => m.partial === false,
  });
  console.log(`status  ${fresh.id}: ${summaryLine(statusMeta)}`);

  // --- /configuration: fields exclusive to the operational source -----------
  const cfgMeta = record(getJSON(`/resources/${fresh.id}/configuration`, 'configuration'), 'configuration');
  check(cfgMeta, {
    '/configuration is operational-only': (m) => m.routingDecision === 'OPERATIONAL',
    // Rule 4 (fields.operational_only) PINS the source and falls through rather
    // than terminating, so the id that reaches the envelope is a TTL rule.
    // Terminating at rule 4 would skip the max_stale ceiling for the one request
    // type that has nowhere else to go. Rule 4 emits its own id only when the
    // operational source is unavailable, and then with routingDecision NONE —
    // which this assertion would not see, because the request would have failed.
    // A cache hit re-reports the stored rule id, which is still a TTL rule.
    '/configuration resolves through a TTL rule, not fields.operational_only': (m) =>
      m.routingRule !== RULE_OPS_ONLY && m.routingRule.indexOf('ttl.') === 0,
  });

  // --- /resources/{id}: the wider operational read --------------------------
  const readMeta = record(getJSON(`/resources/${fresh.id}`, 'read'), 'read');
  check(readMeta, {
    '/resources/{id} answered': (m) => m.sources.length > 0,
    // With a healthy, freshly-touched operational source this is a complete
    // answer. It becomes a 206 only when the execution source has to stand in:
    // resource_read wants configuration, owner, metrics and topology, and the
    // EDS is a catalogued supplier of none of them.
    '/resources/{id} is complete while the operational source is healthy': (m) =>
      m.routingDecision === 'OPERATIONAL' && m.partial === false,
  });

  // --- /details: the fan-out ------------------------------------------------
  const detailsMeta = record(getJSON(`/resources/${fresh.id}/details`, 'details'), 'details');
  check(detailsMeta, {
    '/details reads both sources': (m) => m.routingDecision === 'BOTH',
    '/details fires fields.span_both': (m) => m.routingRule === RULE_SPAN_BOTH,
    '/details records provenance': (m) => m.provenance && Object.keys(m.provenance).length > 0,
  });
  console.log(`details ${fresh.id}: ${summaryLine(detailsMeta)}`);

  // --- execution endpoints --------------------------------------------------
  const histMeta = record(getJSON(`/resources/${fresh.id}/executions?limit=25`, 'executions'), 'executions');
  check(histMeta, {
    '/executions is execution-only': (m) => m.routingDecision === 'EXECUTION',
    '/executions fires fields.execution_only': (m) => m.routingRule === RULE_EXEC_ONLY,
    // execution_history is configured ttl: 0s / cache_ttl: 0s — always live.
    '/executions is never cached': (m) => m.cache.hit === false,
  });

  const execMeta = record(
    getJSON(`/resources/${fresh.id}/executions/${executionIdFor(fresh)}`, 'execution_status'),
    'execution_status',
  );
  check(execMeta, {
    '/executions/{id} is execution-only': (m) => m.routingDecision === 'EXECUTION',
    // execution_status is configured consistency: strong, and a strongly
    // consistent request bypasses the cache READ entirely. Despite its
    // cache_ttl: 2s this endpoint is never answered from cache, so a hit here
    // would mean the bypass regressed.
    '/executions/{id} is never answered from cache': (m) => m.cache.hit === false,
  });

  // --- the in-flight bridge -------------------------------------------------
  // The operational source seeds an in-flight execution reference when i%5==1.
  // resolve_in_flight_execution means /status consults the execution source for
  // those, so /status and /details cannot report different statuses.
  if (INFLIGHT_RESOURCES.length > 0) {
    const r = INFLIGHT_RESOURCES[0];
    const s = record(getJSON(`/resources/${r.id}/status`, 'status'), 'status');
    const d = record(getJSON(`/resources/${r.id}/details`, 'details'), 'details');
    check({ s, d }, {
      'in-flight resource: /status and /details agree on provenance.status': (o) =>
        !o.s || !o.d || !o.s.provenance || !o.d.provenance ||
        o.s.provenance.status === o.d.provenance.status,
    });
    console.log(`in-flight ${r.id}: status -> ${summaryLine(s)}`);
  }

  // --- tenant isolation -----------------------------------------------------
  // R002 belongs to globex (2 % 3 == 2). Asking for it as `local` must not work.
  const foreign = http.get(`${BASE_URL}/api/v1/resources/R002/status`, {
    headers: { 'X-Tenant-ID': 'local', Accept: 'application/json' },
    tags: { route: 'isolation' },
    responseCallback: http.expectedStatuses(404),
  });
  check(foreign, { "another tenant's resource is not readable": (r) => r.status === 404 });

  sleep(1);
}

export function teardown() {
  // Leave the reference sources as we found them, so a subsequent profile does
  // not inherit this one's state.
  chaos.reset();
}

// seconds parses a Go duration string as printed by time.Duration.String(),
// including compound forms such as "2m0s" and "1h30m0s", which is what
// /config/routing returns for max_stale and the longer TTLs.
const UNIT_SECONDS = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

export function seconds(goDuration) {
  if (!goDuration) return 0;
  const re = /([0-9]*\.?[0-9]+)(ns|us|µs|ms|h|m|s)/g;
  let total = 0;
  let matched = false;
  let m;
  while ((m = re.exec(goDuration)) !== null) {
    matched = true;
    total += Number.parseFloat(m[1]) * UNIT_SECONDS[m[2]];
  }
  return matched ? total : Number.parseFloat(goDuration) || 0;
}

// Referenced so a lint pass does not flag the import as unused when the
// in-flight block is disabled.
export const _resources = LOCAL_RESOURCES.length;
