# TTL-Aware BFF — Frozen Design Contract

This file is the single source of truth that every other document, manifest and
package in this repository is written against. It is intentionally terse: it
fixes *names*, *paths*, *keys* and *numbers* so that specs, code, deployment
manifests and diagrams cannot drift apart.

Module path: `github.com/udaykishore/ttl-aware-bff`
Go: `1.24`

## 1. Binaries

| Binary | Path | Purpose | Ports |
|---|---|---|---|
| `bff` | `cmd/bff` | The BFF itself | `8080` HTTP API, `9090` admin (`/healthz`,`/readyz`,`/livez`,`/metrics`) |
| `opsource` | `cmd/opsource` | Reference **Operational Data Source** stub (gRPC) | `9101` gRPC, `9111` admin/chaos HTTP (`/healthz`,`/readyz`,`/livez`) |
| `exsource` | `cmd/exsource` | Reference **Execution Data Source** stub (REST) | `9102` HTTP, `9112` admin/chaos HTTP (`/healthz`,`/readyz`,`/livez`) |

Both stubs answer the same three probes the BFF does, so `docker-compose.yaml` and
the Kubernetes manifests can target `/readyz` uniformly.

## 2. Package map (`internal/`)

```
api/            HTTP server, router wiring, handlers, middleware, response encoding
application/    Use-case orchestration (application.Service) — the request lifecycle
classifier/     Request -> RequestType + RequiredFields + Consistency
domain/         Canonical model. No source types. No transport types.
router/         DataSourceRouter: policy chain producing a RoutingDecision
policy/         SourcePrecedencePolicy, field catalog, consistency policy
freshness/      TTL/Freshness Manager (probe, evaluate, clock-skew correction)
datasource/     Port interfaces + adapters
  operational/  gRPC adapter + generated stubs (opsv1)
  execution/    REST adapter
mapper/         OperationalMapper / ExecutionMapper -> canonical
aggregation/    Concurrent fan-out, per-source timeouts, partial results
cache/          Cache-aside: L1 in-process + L2 Redis, singleflight, negative cache
resilience/     Timeout, bounded retry, circuit breaker, bulkhead, rate limit
observability/  OTel tracer/meter providers, metric instruments, slog logging
security/       JWT verification, RBAC, tenant resolution, redaction
config/         Layered config: file -> env -> tenant overrides, hot reload
testutil/       Assertions, fakes, clocks
```

`pkg/` holds the only two things intended for reuse outside this service:
`pkg/correlation` (correlation-id + tenant context propagation) and `pkg/errs`
(the typed error taxonomy shared by adapters and the API error model).

## 3. Public API (v1)

Base path `/api/v1`. All responses are `application/json`.

| Method | Path | Classification | Typical routing |
|---|---|---|---|
| GET | `/api/v1/resources/{resourceId}` | `resource_read` | OPERATIONAL, fallback EXECUTION |
| GET | `/api/v1/resources/{resourceId}/status` | `resource_status` | OPERATIONAL (TTL 10s) |
| GET | `/api/v1/resources/{resourceId}/configuration` | `resource_configuration` | OPERATIONAL (TTL 30s) |
| GET | `/api/v1/resources/{resourceId}/executions` | `execution_history` | EXECUTION (TTL 0 = always live) |
| GET | `/api/v1/resources/{resourceId}/executions/{executionId}` | `execution_status` | EXECUTION (TTL 5s) |
| GET | `/api/v1/resources/{resourceId}/details` | `resource_details` | BOTH (fan-out) |

Every success response is an envelope:

```jsonc
{
  "data": { /* canonical model */ },
  "meta": {
    "correlationId": "…",
    "routingDecision": "OPERATIONAL|EXECUTION|BOTH|NONE",
    "routingRule": "ttl.operational.fresh",
    "sources": ["OPERATIONAL"],
    "freshness": {
      "state": "FRESH|STALE|UNKNOWN",
      "ageSeconds": 12.4,
      "ttlSeconds": 30,
      "observedAt": "…",
      "evaluatedAt": "…",
      "source": "OPERATIONAL",
      "skewCorrected": false,
      "version": 41
    },
    "degraded": false,
    "partial": false,
    "cache": { "hit": true, "layer": "L1|L2|NONE", "ageMs": 900 },
    "provenance": { "status": "OPERATIONAL", "latestExecution": "EXECUTION" },
    "warnings": [ { "code": "…", "message": "…", "source": "EXECUTION" } ],
    "elapsedMs": 6
  }
}
```

`domain.Freshness` carries a custom `MarshalJSON`/`UnmarshalJSON` pair: `Age` and
`TTL` are `time.Duration` internally and would otherwise serialise as opaque
nanosecond integers, so they are emitted as `ageSeconds` and `ttlSeconds`. Both
are **always** present — there is no `omitempty` — so `state: UNKNOWN` with
`ageSeconds: 0` means "no age could be established", not "brand new". The pair
round-trips, so a cache entry written by one replica reports the same age when
another serves it.

Warning codes: `STALE_DATA`, `SOURCE_TIMEOUT`, `SOURCE_UNAVAILABLE`,
`PARTIAL_DATA`, `CONFLICT_RESOLVED`, `SCHEMA_VERSION_MISMATCH`,
`CLOCK_SKEW_DETECTED`, `CACHE_UNAVAILABLE`. A `SOURCE_UNAVAILABLE` or
`SOURCE_TIMEOUT` warning names the source that **failed**, never the one that
answered.

HTTP status is derived from `meta.partial` alone: any partial answer is `206`, a
degraded-but-complete one stays `200`. `meta.partial` is set both when a source
the request wanted did not answer and when the source that did cannot hold every
requested field — which is what makes a fallback answer for `/resources/{id}` a
`206`.

Per-source states in an error document: `HEALTHY`, `CIRCUIT_OPEN`,
`CIRCUIT_HALF_OPEN`, `SATURATED`, `UNCONFIGURED`.

Error responses (RFC 7807-shaped):

```jsonc
{
  "error": {
    "code": "UPSTREAM_UNAVAILABLE",
    "type": "https://errors.bff.internal/upstream-unavailable",
    "title": "Upstream data source unavailable",
    "status": 503,
    "detail": "…",
    "correlationId": "…",
    "retryable": true,
    "sources": [ { "source": "OPERATIONAL", "state": "CIRCUIT_OPEN" } ]
  }
}
```

Error codes: `INVALID_REQUEST`, `UNAUTHENTICATED`, `FORBIDDEN`, `TENANT_MISMATCH`,
`NOT_FOUND`, `RATE_LIMITED`, `REQUEST_TOO_LARGE`, `UPSTREAM_TIMEOUT`,
`UPSTREAM_UNAVAILABLE`, `UPSTREAM_INVALID_RESPONSE`, `SCHEMA_VERSION_MISMATCH`,
`NO_SOURCE_AVAILABLE`, `INTERNAL`.

Headers: `X-Correlation-ID` (in/out), `X-Tenant-ID` (optional, must match JWT),
`X-Request-ID`, `Authorization: Bearer <jwt>`, `traceparent` (W3C).

## 4. Canonical domain model (`internal/domain`)

`Resource`, `ResourceStatus`, `Owner`, `Metric`, `Topology`,
`Execution`, `ExecutionStatus`, `WorkflowStep`, `Action`, `ExecutionResult`,
`ExecutionError`, `AuditEntry`, `ResourceDetails`,
`Freshness`, `FreshnessState`, `SourceKind`, `Warning`, `CacheInfo`, `CacheLayer`,
`ExecutionList`, `ResponseMeta`. Provenance is not a type: it is
`ResponseMeta.Provenance`, a `map[string]SourceKind` keyed by canonical field name.

`SourceKind` ∈ `OPERATIONAL | EXECUTION | CACHE | NONE`.
`FreshnessState` ∈ `FRESH | STALE | UNKNOWN`.
`ResourceStatus` ∈ `PENDING | ACTIVE | SUSPENDED | DEGRADED | TERMINATING | TERMINATED | ERROR | UNKNOWN`.
`ExecutionStatus` ∈ `QUEUED | RUNNING | COMPLETED | FAILED | CANCELLED | TIMED_OUT | UNKNOWN`.

## 5. Routing decision

```go
type Target int // TargetNone | TargetOperational | TargetExecution | TargetBoth
type Decision struct {
    Target        Target
    Rule          string        // stable id, emitted as metric/trace attribute
    Reason        string
    Primary       domain.SourceKind
    Fallback      domain.SourceKind
    OperationalTTL time.Duration  // source freshness TTL
    CacheTTL       time.Duration  // response cache lifetime — a different concept
    AllowStale    bool          // the EFFECTIVE allowance: per-type OR tenant default
    MaxStale      time.Duration
    PerSourceTimeout map[domain.SourceKind]time.Duration
    RequiredSources  map[domain.SourceKind]bool // false => optional (partial ok)
    Freshness     freshness.Evaluation          // the verdict the decision used
    ProbeFailed   bool
    Consistency   classifier.Consistency
}
```

Rule chain, evaluated in order; first match wins. Rule ids are stable strings:

0. `guard.unconfigured_request_type` → NONE (503). **Not a rule**: a pre-chain
   exit taken when the request type has no routing rule at all. Emitted to
   `routing_decision_total` like any other decision, because a deployment that
   has lost a request type must be visible in the metric an operator reads first.
1. `guard.tenant_missing` → NONE (400)
2. `health.both_unavailable` → NONE unless stale-serve allowed
3. `fields.execution_only` → **pins** the source to EXECUTION and **clears the
   configured fallback** (a fallback that cannot supply the requested fields is a
   wrong answer, not a fallback), then terminates: EXECUTION, or NONE if the EDS
   is down.
4. `fields.operational_only` → pins the same way, but *continues* the chain, so
   rules 8/9/10 still decide and the `max_stale` ceiling still applies.
   Terminates — emitting its own id with `Target = NONE` — only when the ODS is
   unavailable. Either pin also binds rule 10, which may not cross it.
5. `fields.span_both` → BOTH
6. `consistency.strong_requires_operational` → OPERATIONAL, read live, with the
   cache **read** bypassed entirely (the result is still written back)
7. `health.primary_unavailable` → fallback source; response is `degraded: true`
   with a `SOURCE_UNAVAILABLE` warning **naming the source that failed**.
   `preferred_source: both` is matched on the configured string, since "both" is
   not a `SourceKind`, which is what keeps the one-side-healthy branch reachable.
8. `ttl.operational.fresh` → OPERATIONAL
9. `ttl.operational.stale` → EXECUTION (or OPERATIONAL+degraded when `allow_stale`)
10. `ttl.unknown_freshness` → configured `on_unknown_freshness`. `none` is
    honoured and yields NONE; only an *unparseable* value falls back to
    `operational`. Crossing to the other source is refused when a field rule
    pinned the set.
11. `default.preferred_source` → configured preferred source

Two further ids are **not** part of the chain and are stamped by the application
layer:

- `fallback.primary_failed` — the preferred source failed *during the call*, after
  a decision had already been made. Gated on `errs.SourceUnusable`
  (`UPSTREAM_TIMEOUT`, `UPSTREAM_UNAVAILABLE`, `UPSTREAM_INVALID_RESPONSE`,
  `SCHEMA_VERSION_MISMATCH`); refused for strongly-consistent requests and where
  the fallback is `none` or equals the primary. `degraded: true`.
- `degrade.stale_cache` — every source refused and an expired-but-physically-resident
  cache entry was served instead. `Target = NONE`, `degraded: true`. Gated on the
  same `errs.SourceUnusable` predicate as the fallback (plus
  `NO_SOURCE_AVAILABLE`), and it carries the cached entry's `provenance`,
  `warnings` and `partial` forward. Exported as `application.RuleDegradeStaleCache`.

## 6. Configuration keys (`configs/bff.yaml`)

```yaml
server: { http_addr, admin_addr, read_timeout, write_timeout, idle_timeout, shutdown_grace, max_body_bytes }
security: { jwt: { issuer, audience, jwks_url, hs256_secret_env, leeway, required_claims }, rbac: { roles }, rate_limit: { rps, burst, per_tenant } }
sources:
  operational: { addr, dial_timeout, call_timeout, freshness_probe_timeout, max_conns, keepalive, tls: {...},
                 retry: {max_attempts, base_backoff, max_backoff, jitter}, breaker: {...}, bulkhead: {max_concurrent, acquire_timeout} }
  execution:   { base_url, call_timeout, ..., same shape }
cache: { enabled, backend: memory|redis|layered, redis: {addr, db, password_env, pool_size, dial_timeout}, l1: {enabled, max_entries, ttl},
         key_prefix, negative_ttl, stale_grace: 5m, fail_open: true,
         stampede: {enabled, lock_ttl, early_refresh_ratio} }
routing:
  defaults: { on_unknown_freshness: operational, clock_skew_tolerance: 2s, allow_stale: true, max_stale: 5m,
              probe_enabled: true, probe_cache_ttl: 1s,
              resolve_in_flight_execution: true, in_flight_lookup_timeout: 300ms }
  request_types:
    resource_status:        { preferred_source: operational, ttl: 10s, cache_ttl: 3s,  fallback: execution, allow_stale: true,  max_stale: 120s }
    resource_configuration: { preferred_source: operational, ttl: 30s, cache_ttl: 15s, fallback: none,      allow_stale: true,  max_stale: 300s }
    resource_read:          { preferred_source: operational, ttl: 30s, cache_ttl: 5s,  fallback: execution, allow_stale: true,  max_stale: 300s }
    execution_status:       { preferred_source: execution,   ttl: 5s,  cache_ttl: 2s,  fallback: none,      allow_stale: false }
    execution_history:      { preferred_source: execution,   ttl: 0s,  cache_ttl: 0s,  fallback: none,      allow_stale: false }
    resource_details:       { preferred_source: both,        ttl: 30s, cache_ttl: 5s,  fallback: operational, allow_stale: true, max_stale: 300s,
                              required_sources: { operational: true, execution: false } }
precedence:
  fields: { status: [operational, execution], configuration: [operational], metrics: [operational], topology: [operational],
            owner: [operational], latestExecution: [execution], executionHistory: [execution], lastOperation: [execution] }
  execution_overrides_when_running: [ status, subState ]
tenants:
  acme: { routing: { request_types: { resource_status: { ttl: 5s } } }, cache: { ... }, security: { ... } }
observability: { service_name, service_version, environment, otlp: {endpoint, insecure, timeout}, trace_sample_ratio, metrics_interval, log: {level, format} }
```

Three keys are easy to miss and each governs a behaviour documented elsewhere:

| Key | Default | Governs |
|---|---|---|
| `routing.defaults.resolve_in_flight_execution` | `true` | An operational-only read consults the execution source when the operational record declares an `in_flight_execution_ref`, so `/status` and `/details` cannot disagree mid-workflow. Best-effort, and the marker alone confers no precedence authority — the override needs an execution candidate that is actually in progress. A `*bool` (as are `allow_stale` and `probe_enabled`), so a tenant can turn one off without also setting a companion duration; the accessors `StaleAllowed()`, `ProbesEnabled()` and `InFlightResolutionEnabled()` all default to true |
| `routing.defaults.in_flight_lookup_timeout` | `300ms` | The budget for that extra call |
| `cache.stale_grace` | `5m` | How long an entry is *physically* retained past its logical `cache_ttl` so the stale-serve path can still reach it. Only `Manager.GetStale` sees it |

Environment overrides use prefix `BFF_` with `__` as the nesting separator, e.g.
`BFF_SOURCES__OPERATIONAL__ADDR`, `BFF_ROUTING__REQUEST_TYPES__RESOURCE_STATUS__TTL`.
No TTL is ever compiled into Go source (REQ-CFG-001).

Cache keys are
`<key_prefix>:e<EntrySchemaVersion>:t=<tenant>:rt=<requestType>:r=<resource>[:s=<sub>][:v=<hash>]`.
`cache.TenantPrefix` ends **with** the `:` delimiter, so flushing tenant `acme`
cannot spill into `acme2`; the empty-segment placeholder is `~`.

## 7. Metrics (OTel, exported via OTLP and `/metrics`)

Counters: `bff_request_total`, `operational_ttl_hit_total`, `operational_ttl_miss_total`,
`execution_fallback_total`, `datasource_error_total`, `cache_hit_total`,
`cache_miss_total`, `cache_error_total`, `partial_response_total`,
`stale_response_total`, `routing_decision_total`,
`circuit_breaker_transition_total`, `bulkhead_rejected_total`,
`rate_limited_total`, `precedence_conflict_total`,
`schema_version_mismatch_total`, `clock_skew_detected_total`,
`config_reload_total`.

`schema_version_mismatch_total` is the *only* signal for an unsupported source
contract: a schema mismatch deliberately does not move
`circuit_breaker_state`/`circuit_breaker_transition_total`, because the source is
healthy and it is the BFF that cannot read it. It, and every other client fault,
**abstains** from the breaker — `Breaker.Do` returns without recording an outcome
at all, rather than recording a success, so a source answering nothing but 404s
while genuinely down cannot satisfy the half-open threshold and be re-admitted.

Histograms (seconds): `bff_request_latency`, `operational_source_latency`,
`execution_source_latency`, `aggregation_latency`, `data_freshness_age`.

Gauges/UpDown: `bff_concurrent_requests`, `bulkhead_in_flight`, `circuit_breaker_state`.

Common attributes: `tenant_id`, `request_type`, `routing_decision`, `routing_rule`,
`source`, `outcome`, `http_status`, `degraded`, `partial`.

## 8. Spans

The server span is named by route **pattern** (`GET /api/v1/resources/{resourceId}/status`),
never by path, so trace cardinality stays bounded. Inside it the service emits
exactly five spans:

| Span | Emitted by |
|---|---|
| `bff.usecase.resource` | `application.Service.resourceView` (attrs `request_type`, `view`) |
| `bff.usecase.execution` | `application.Service.executionView` (attr `request_type`) |
| `bff.route` | `application.Service.route` (attrs `routing_target`, `routing_rule`, `freshness`) |
| `bff.aggregate` | `application.Service.fetch` (attrs `routing_target`, `routing_rule`) |
| `bff.resolve_in_flight` | `application.Service.resolveInFlight`, **only** when the operational record declared an `in_flight_execution_ref` (attr `execution_id`) |

There is deliberately no span per source call, per mapper or per precedence
decision; those are metrics.

## 9. Deployment names

Docker images: `ghcr.io/udaykishore/ttl-aware-bff:{tag}`,
`ghcr.io/udaykishore/ttl-aware-bff-opsource:{tag}`,
`ghcr.io/udaykishore/ttl-aware-bff-exsource:{tag}`.
K8s namespace `bff`, Deployment/Service `ttl-aware-bff`, Helm chart `ttl-aware-bff`.
Terraform stack provisions VPC, EKS, ElastiCache (Redis), ALB, IRSA, CloudWatch,
ADOT collector.
