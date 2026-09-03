# TTL-Aware BFF — Requirements Catalogue

Normative specification for `github.com/udaykishore-resu/ttl-aware-bff`. This document
is the root of traceability: every other file under `spec/`, every package under
`internal/`, and every test under `test/` cites the IDs defined here. IDs are
**stable and never renumbered**; a withdrawn requirement is marked `WITHDRAWN`
and its number is retired.

Keywords `MUST`, `MUST NOT`, `SHOULD`, `SHOULD NOT`, `MAY` are RFC 2119.

Authoritative names, paths, endpoints, config keys, metric names, rule ids and
error codes come from `docs/DESIGN-CONTRACT.md`. Where this document appears to
disagree with the contract, the contract wins and this document is a defect.

**Verified by** columns name a Go test function and the file that holds it.
Package-relative paths are rooted at the module root.

---

## 0. Requirement families and scope

| Family | Domain | Primary package(s) |
|---|---|---|
| `REQ-API-*` | HTTP surface, envelope, status codes, headers | `internal/api`, `internal/api/handler`, `internal/api/response` |
| `REQ-CLS-*` | Request classification | `internal/classifier` |
| `REQ-RT-*` | Routing rule chain and decisions | `internal/router` |
| `REQ-TTL-*` | Freshness/TTL evaluation, probes, skew | `internal/freshness` |
| `REQ-PREC-*` | Source precedence and conflict resolution | `internal/policy` |
| `REQ-AGG-*` | Concurrent fan-out and partial results | `internal/aggregation` |
| `REQ-MAP-*` | Source schema → canonical mapping | `internal/mapper` |
| `REQ-DS-*` | Data source ports and adapters | `internal/datasource/**` |
| `REQ-CACHE-*` | L1/L2 cache-aside, stampede, negative cache | `internal/cache` |
| `REQ-RES-*` | Timeout, retry, breaker, bulkhead, rate limit | `internal/resilience` |
| `REQ-OBS-*` | Metrics, traces, logs | `internal/observability` |
| `REQ-SEC-*` | AuthN/AuthZ, TLS, redaction, validation | `internal/security`, `internal/api/middleware` |
| `REQ-MT-*` | Multi-tenancy and isolation | cross-cutting |
| `REQ-CFG-*` | Configuration, overrides, hot reload | `internal/config` |
| `REQ-PERF-*` | Latency and throughput budgets | cross-cutting |
| `REQ-EDGE-*` | The twenty mandated edge cases | cross-cutting |

---

## 1. REQ-API — Public API surface

### REQ-API-001 — Endpoint set is closed
**MUST.** The service exposes exactly six versioned resource endpoints under base
path `/api/v1`, plus the admin surface on the admin port:

| Method | Path | Classification |
|---|---|---|
| GET | `/api/v1/resources/{resourceId}` | `resource_read` |
| GET | `/api/v1/resources/{resourceId}/status` | `resource_status` |
| GET | `/api/v1/resources/{resourceId}/configuration` | `resource_configuration` |
| GET | `/api/v1/resources/{resourceId}/executions` | `execution_history` |
| GET | `/api/v1/resources/{resourceId}/executions/{executionId}` | `execution_status` |
| GET | `/api/v1/resources/{resourceId}/details` | `resource_details` |

Any other path under `/api/v1` MUST produce `404` with error code `NOT_FOUND`.
Any non-GET method on a defined path MUST produce `405`.

*Rationale.* A closed surface makes the classifier total (REQ-CLS-002) and lets
routing config be keyed by an enumerable `request_type` set.

**Verified by** `TestRouter_EndpointSetIsClosed`, `TestRouter_MethodNotAllowed` in `internal/api/router_test.go`.

### REQ-API-002 — Admin surface separated from data plane
**MUST.** `/healthz`, `/readyz`, `/livez` and `/metrics` are served on the admin
listener (`server.admin_addr`, default `:9090`) and MUST NOT be reachable on the
data-plane listener (`:8080`).

*Rationale.* Prevents scrape and probe traffic from consuming data-plane
bulkhead/rate-limit budget, and keeps `/metrics` off the public ingress.

**Verified by** `TestAdmin_NotServedOnDataPlane` in `internal/api/router_test.go`.

### REQ-API-003 — Readiness reflects dependency readiness, liveness does not
**MUST.** `/livez` returns `200` whenever the process event loop is responsive.
`/readyz` returns `200` only when configuration has loaded and at least one data
source port is usable (health `SERVING` or `DEGRADED`) or stale-serve is enabled
with a warm cache; otherwise `503`.

*Rationale.* An unready-but-alive pod must be removed from the load balancer, not
restarted (restarting on upstream failure amplifies the outage).

**Verified by** `TestHealth_ReadyzReflectsSources`, `TestHealth_LivezIndependent` in `internal/api/handler/health_test.go`.

### REQ-API-004 — Canonical success envelope
**MUST.** Every `2xx` response body is exactly:

```json
{ "data": { }, "meta": { } }
```

with `meta` carrying `correlationId`, `routingDecision`, `routingRule`,
`sources`, `freshness`, `degraded`, `partial`, `cache`, `provenance`, and
`warnings`. `data` MUST contain only canonical domain types (REQ-MAP-001). No
source-native field name may appear anywhere in the response body.

*Rationale.* A single envelope shape lets the UI implement freshness/degradation
affordances once rather than per endpoint.

**Verified by** `TestEnvelope_ShapeIsStable`, `TestEnvelope_NoSourceNativeFields` in `internal/api/response/envelope_test.go`.

### REQ-API-005 — `meta.freshness` is always present and self-describing
**MUST.** `meta.freshness` carries `state` ∈ `FRESH|STALE|UNKNOWN`,
`ageSeconds` (float, skew-corrected, REQ-TTL-004), `ttlSeconds` (the effective
TTL that was applied, after tenant override), and, when known, `observedAt`,
`evaluatedAt`, `source`, `skewCorrected` and `version`.

`ageSeconds` and `ttlSeconds` are produced by a custom `MarshalJSON` on
`domain.Freshness`, because the underlying `Age` and `TTL` are `time.Duration`
and `encoding/json` would otherwise render them as opaque nanosecond integers.
The matching `UnmarshalJSON` restores them, so the pair survives a round trip
through a cache entry and a cached answer reports the same age a live one would.

Both fields are **always emitted** — there is no `omitempty`. When routing was
`NONE`, or the data came from a source with no freshness envelope, `state` is
`UNKNOWN` and `ageSeconds` is `0`.

*Rationale.* `state` is what distinguishes "zero seconds old" from "age unknown";
a client that reads `ageSeconds` without reading `state` will display false
confidence. Making `state` load-bearing rather than making `ageSeconds` optional
keeps the schema total: every consumer gets a number of the same type in every
response, and the one flag that qualifies it is required too.

**Verified by** `TestFreshness_JSONRoundTrip` in `internal/domain/domain_test.go`,
which asserts both fields are present and survive a decode.

### REQ-API-006 — `meta.provenance` is per-field-group
**MUST.** `provenance` maps a canonical field-group name (`status`,
`configuration`, `metrics`, `topology`, `owner`, `latestExecution`,
`executionHistory`, `lastOperation`) to the `SourceKind` that actually supplied
the value in this response. Groups absent from `data` MUST be absent from
`provenance`.

*Rationale.* Precedence (REQ-PREC-001) is invisible without provenance; support
engineers need to know which source produced a disputed value.

**Verified by** `TestProvenance_ReflectsWinningSource` in `internal/policy/precedence_test.go`.

### REQ-API-007 — `degraded` and `partial` are orthogonal flags
**MUST.** `meta.degraded = true` iff the response contains data that failed the
freshness requirement (stale-serve) or came from a fallback source after a
primary failure. `meta.partial = true` iff at least one *optional* source in the
routing decision failed or timed out and its contribution is missing. Both MAY be
true simultaneously.

*Rationale.* "Old but complete" and "fresh but incomplete" require different
client behaviour.

**Verified by** `TestMeta_DegradedPartialOrthogonal` in `internal/application/resource_service_test.go`.

### REQ-API-008 — Status code mapping
**MUST.** Response status is chosen as: `200` for complete or degraded-but-whole
responses; `206` when `meta.partial = true` and a *required* field group is
missing; `404` `NOT_FOUND` when every consulted source reports the resource
absent; `503` `NO_SOURCE_AVAILABLE` when no source produced data and stale-serve
could not satisfy the request. Full table in `spec/error-model.md`.

*Rationale.* Degradation is a successful answer with caveats; absence of any
answer is not.

**Verified by** `TestStatusMapping_Table` in `internal/api/response/status_test.go`.

### REQ-API-009 — Error body is RFC 7807-shaped and closed over codes
**MUST.** Errors are emitted as `{"error": {code, type, title, status, detail,
correlationId, retryable, sources}}`. `code` MUST be one of: `INVALID_REQUEST`,
`UNAUTHENTICATED`, `FORBIDDEN`, `TENANT_MISMATCH`, `NOT_FOUND`, `RATE_LIMITED`,
`REQUEST_TOO_LARGE`, `UPSTREAM_TIMEOUT`, `UPSTREAM_UNAVAILABLE`,
`UPSTREAM_INVALID_RESPONSE`, `SCHEMA_VERSION_MISMATCH`, `NO_SOURCE_AVAILABLE`,
`INTERNAL`. `type` MUST be `https://errors.bff.internal/<kebab-code>`.

*Rationale.* Clients switch on `code`; a closed set makes client error handling
exhaustive and lintable.

**Verified by** `TestErrorCodes_ClosedSet`, `TestErrorType_URIDerivation` in `pkg/errs/errs_test.go`.

### REQ-API-010 — Correlation id round-trip
**MUST.** `X-Correlation-ID` is accepted from the client, validated against
`^[A-Za-z0-9._-]{1,128}$`, and echoed in the response header and in
`meta.correlationId` / `error.correlationId`. When absent or invalid, the BFF
generates a UUIDv4 and uses that. The value MUST be propagated to both sources
(ODS `RequestContext.correlation_id`; EDS `X-Correlation-ID` header).

*Rationale.* End-to-end correlation across two heterogeneous protocols is the
primary debugging tool for routing decisions.

**Verified by** `TestCorrelation_RoundTrip`, `TestCorrelation_RejectsMalformed` in `pkg/correlation/correlation_test.go`.

### REQ-API-011 — W3C trace context propagation
**MUST.** Inbound `traceparent`/`tracestate` are honoured as the parent span
context; outbound calls to ODS and EDS MUST carry propagated context.

**Verified by** `TestTracePropagation_InboundOutbound` in `internal/observability/propagation_test.go`.

### REQ-API-012 — Response is deterministic for a fixed input
**MUST.** For a fixed (tenant, path, clock, source responses, cache state) the
serialized response body MUST be byte-identical across runs. Map-valued fields
(configuration, labels) MUST serialize with sorted keys; slices MUST have a
defined ordering (executions newest-first by `startedAt`, then `executionId`).

*Rationale.* Non-determinism defeats golden-file tests and makes ETag/caching
impossible later.

**Verified by** `TestEnvelope_GoldenDeterminism` in `internal/api/response/golden_test.go`.

### REQ-API-013 — Content negotiation and encoding
**MUST.** Responses are `application/json; charset=utf-8`. `gzip` is applied when
the client advertises it and the body exceeds 1 KiB. Timestamps are RFC 3339 with
UTC offset `Z` and millisecond precision.

**Verified by** `TestEncoding_ContentTypeAndTimestamps` in `internal/api/response/envelope_test.go`.

### REQ-API-014 — Query parameters on collection endpoints
**MUST.** `/api/v1/resources/{resourceId}/executions` accepts `limit`
(1..200, default 50) and `cursor` (opaque). Out-of-range `limit` MUST produce
`400 INVALID_REQUEST`; it MUST NOT be silently clamped.

*Rationale.* Silent clamping hides client bugs and makes pagination totals lie.

**Verified by** `TestExecutions_LimitValidation` in `internal/api/handler/executions_test.go`.

### REQ-API-015 — Warnings are structured, not prose
**MUST.** `meta.warnings[]` entries are `{code, message, source}` where `code` is
drawn from a documented enumeration (`STALE_DATA`,
`SOURCE_TIMEOUT`, `SOURCE_UNAVAILABLE`, `CONFLICT_RESOLVED`,
`CLOCK_SKEW_DETECTED`, `SCHEMA_VERSION_MISMATCH`, `STALE_DATA`,
`PARTIAL_DATA`, `CONFLICT_RESOLVED`, `CLOCK_SKEW_DETECTED`, `CACHE_UNAVAILABLE`). `message` is human-facing and MUST NOT
contain tenant-identifying data.

**Verified by** `TestWarnings_CodesEnumerated` in `internal/api/response/warning_test.go`.

---

## 2. REQ-CLS — Request classification

### REQ-CLS-001 — Classification is derived from route, not from body or heuristics
**MUST.** `internal/classifier` maps `(method, matched route template)` to a
`RequestType` constant. Classification MUST NOT inspect the request body, query
values other than declared ones, or user-agent.

*Rationale.* Deterministic classification is a precondition for a deterministic
routing decision and for config keyed by `request_type`.

**Verified by** `TestClassifier_RouteToRequestType` in `internal/classifier/classifier_test.go`.

### REQ-CLS-002 — Classification is total
**MUST.** Every route registered under REQ-API-001 has exactly one
`RequestType`. An unmatched route never reaches the classifier. A classifier
returning an unknown `RequestType` is a programming error and MUST panic in test
builds and return `INTERNAL` in production builds.

**Verified by** `TestClassifier_TotalOverRoutes` in `internal/classifier/classifier_test.go`.

### REQ-CLS-003 — Classifier emits RequiredFields
**MUST.** The classifier produces a `RequiredFields` set drawn from the field
catalog in `internal/policy` (`status`, `configuration`, `metrics`, `topology`,
`owner`, `latestExecution`, `executionHistory`, `lastOperation`,
`subState`). This set is the input to routing rules 3–5.

*Rationale.* Routing by *fields needed* rather than by endpoint name is what lets
one endpoint span both sources without a hand-written special case.

**Verified by** `TestClassifier_RequiredFieldsPerType` in `internal/classifier/classifier_test.go`.

### REQ-CLS-004 — Classifier emits a consistency requirement
**MUST.** The classifier produces `Consistency` ∈ `EVENTUAL | STRONG`. `STRONG`
is produced when the client sends `Cache-Control: no-cache` or when the request
type is configured `allow_stale: false`. `STRONG` forbids cache reads and stale
serving and drives routing rule 6.

**Verified by** `TestClassifier_ConsistencyFromNoCache`, `TestClassifier_ConsistencyFromConfig` in `internal/classifier/classifier_test.go`.

### REQ-CLS-005 — Classification is observable
**MUST.** `request_type` is attached as an attribute to the `bff.usecase.*` span, to
every metric emitted downstream, and to the request log line.

**Verified by** `TestClassifier_SpanAttributes` in `internal/classifier/observability_test.go`.

---

## 3. REQ-RT — Routing

### REQ-RT-001 — Routing is a first-match-wins ordered rule chain
**MUST.** `internal/router.DataSourceRouter` evaluates exactly the eleven rules
below, in this order, and returns on the first match.

Before the chain runs, `Select` takes a **pre-chain exit** when the request type
has no routing rule configured at all: rule id `guard.unconfigured_request_type`,
`Target = NONE` → `503`. It is a deployment defect rather than a routing outcome,
and it MUST be reported through the decision hook so that it appears in
`routing_decision_total` like every other decision.

| # | Rule id | Outcome |
|---|---|---|
| — | `guard.unconfigured_request_type` | `NONE` (503) — taken before rule 1, and only when the request type has no routing rule at all |
| 1 | `guard.tenant_missing` | `NONE` (400) |
| 2 | `health.both_unavailable` | `NONE` unless stale-serve allowed |
| 3 | `fields.execution_only` | **Pins** the source to the EDS and clears the configured fallback, then terminates: `EXECUTION`, or `NONE` if the EDS is down |
| 4 | `fields.operational_only` | **Pins** the source to `OPERATIONAL`, clears the configured fallback, and *continues* — rules 8/9/10 decide, and their id is what is emitted. Terminates with `NONE` only when the ODS is down. |
| 5 | `fields.span_both` | `BOTH` |
| 6 | `consistency.strong_requires_operational` | `OPERATIONAL`, read live, with the cache **read** bypassed (the result is still written back) |
| 7 | `health.primary_unavailable` | fallback source, `degraded: true` with a `SOURCE_UNAVAILABLE` warning naming the source that failed. `preferred_source: both` is matched on the configured string, since "both" is not a `SourceKind` |
| 8 | `ttl.operational.fresh` | `OPERATIONAL` |
| 9 | `ttl.operational.stale` | `EXECUTION`, or `OPERATIONAL`+degraded when `allow_stale` |
| 10 | `ttl.unknown_freshness` | configured `on_unknown_freshness`. `none` MUST be honoured (`NONE` → `503`); only an unparseable value falls back to `operational`. A pin from rule 3 or 4 forbids crossing to the other source |
| 11 | `default.preferred_source` | configured preferred source |

Two further rule ids exist and are **not** part of the chain; the application
layer stamps them after routing has already produced a decision:

| Rule id | Stamped by | Outcome |
|---|---|---|
| `fallback.primary_failed` | `application.Service.fallbackDecision` | The preferred source failed *during the call*, before the breaker had learned about it. Gated on `errs.SourceUnusable`; refused for `consistency: strong` and where the fallback is `none` or equals the primary. `degraded: true` with a `SOURCE_UNAVAILABLE` warning naming the source that **failed**. If the fallback also fails, the **primary's** error is what is reported. |
| `degrade.stale_cache` | `application.Service.serveStale`, exported as `application.RuleDegradeStaleCache` | Every source refused and an expired-but-physically-resident entry passed the four gates, the first of which is the same `errs.SourceUnusable` predicate the fallback uses. `Target = NONE`, `degraded: true`, and the cached entry's `provenance`, `warnings` and `partial` are carried forward. |

*Rationale.* Order encodes precedence of concerns: correctness guards, then
availability, then capability, then consistency, then freshness, then defaults.
Rule 4 pins rather than terminates because terminating would skip the TTL rules,
and with them the `max_stale` ceiling, for the one request type
(`resource_configuration`) that has no other source to go to. The ceiling is a
safety property, not an optimisation.

**Verified by** `TestRouter_ChainOrder`, `TestRouter_FirstMatchWins` in `internal/router/router_test.go`.

### REQ-RT-002 — Rule chain is total
**MUST.** Rule 11 (`default.preferred_source`) always matches; the router MUST
NOT be able to return without a rule id. A `Decision` with an empty `Rule` is a
defect.

**Verified by** `TestRouter_AlwaysEmitsRuleID` (property test over randomized inputs) in `internal/router/router_property_test.go`.

### REQ-RT-003 — Routing decides before fetching
**MUST.** For request types whose `preferred_source` is `operational` and whose
`ttl > 0`, the router MUST obtain freshness via the cheap probe
(`GetResourceFreshness`, REQ-DS-002) — or from cached freshness state — *before*
issuing a full read. A full `GetResource` MUST NOT be used merely to learn
freshness.

*Rationale.* The whole point of the design: the expensive read is the thing being
avoided, so freshness must be knowable independently of it.

**Verified by** `TestRouter_ProbeBeforeFullRead`, `TestRouter_NoFullReadForFreshnessOnly` in `internal/router/probe_test.go`.

### REQ-RT-004 — Decision is a value, not a side effect
**MUST.** `Decision` is an immutable value carrying `Target`, `Rule`, `Reason`,
`Primary`, `Fallback`, `OperationalTTL`, `AllowStale`, `MaxStale`,
`PerSourceTimeout`, `RequiredSources`. The router MUST NOT perform I/O other than
the freshness probe and health lookups, and MUST NOT mutate cache.

*Rationale.* A pure decision object is table-testable and can be logged verbatim.

**Verified by** `TestRouter_DecisionIsPure` in `internal/router/router_test.go`.

### REQ-RT-005 — `Reason` is human-readable and includes the deciding inputs
**SHOULD.** `Reason` states the predicate that fired with concrete values, e.g.
`operational age 41.2s > ttl 30s and allow_stale=false`.

**Verified by** `TestRouter_ReasonIncludesInputs` in `internal/router/router_test.go`.

### REQ-RT-006 — Per-source timeouts are part of the decision
**MUST.** `PerSourceTimeout` is populated from
`sources.<source>.call_timeout`, then narrowed to fit the remaining request
deadline. A source whose narrowed timeout would be below
`resilience.min_viable_timeout` (default 25 ms) MUST be dropped from the
decision and recorded as a warning rather than invoked and immediately cancelled.

*Rationale.* Calling an upstream with a 3 ms budget burns a connection, pollutes
the breaker with a guaranteed failure, and returns nothing.

**Verified by** `TestRouter_DropsSourceBelowMinViableTimeout` in `internal/router/timeout_test.go`.

### REQ-RT-007 — Required vs optional sources
**MUST.** `RequiredSources[kind] = true` means failure of that source fails the
field groups it owns; `false` means its absence yields `partial = true` and a
warning but a `200`. For `resource_details` the contract fixes
`{operational: true, execution: false}`.

**Verified by** `TestRouter_RequiredSourcesFromConfig` in `internal/router/router_test.go`.

### REQ-RT-008 — Fallback is single-hop
**MUST.** A fallback source is attempted at most once per request. If the
fallback also fails, the request degrades (stale serve) or fails; it MUST NOT
chain to a third attempt or loop back to the primary.

*Rationale.* Multi-hop fallback multiplies tail latency without improving the
success probability materially.

**Verified by** `TestRouter_FallbackSingleHop` in `internal/router/fallback_test.go`.

### REQ-RT-009 — Decision is emitted as telemetry
**MUST.** Every decision increments `routing_decision_total{routing_decision,
routing_rule, request_type, tenant_id}` and sets `routing.rule` /
`routing.decision` attributes on the `bff.route` span.

**Verified by** `TestRouter_EmitsDecisionMetric` in `internal/router/observability_test.go`.

### REQ-RT-010 — Routing never depends on wall-clock ordering of rules
**MUST.** Rule predicates MUST be pure functions of `(RequestType, RequiredFields,
Consistency, tenant, freshness verdict, source health snapshot, effective config)`.
A health snapshot is taken once per request and reused for the whole chain.

*Rationale.* Reading health twice mid-chain can produce a decision that is
internally inconsistent (e.g. rule 7 says primary is down, rule 8 says it is
fresh).

**Verified by** `TestRouter_HealthSnapshotStability` in `internal/router/router_test.go`.

---

## 4. REQ-TTL — Freshness and TTL

### REQ-TTL-001 — Source freshness TTL is distinct from cache TTL
**MUST.** `routing.request_types.<type>.ttl` (how old source data may be and
still be considered current) and `routing.request_types.<type>.cache_ttl` (how
long the BFF may reuse its own computed response) are separate keys with separate
semantics. Cache entries MUST carry the source `observedAt` so a cache hit can
still be evaluated as `STALE`.

*Rationale.* Conflating them causes the classic bug where a 3 s cache makes 40 s
old upstream data look fresh.

**Verified by** `TestTTL_CacheTTLDistinctFromFreshnessTTL`, `TestCache_HitCanStillBeStale` in `internal/freshness/ttl_test.go`, `internal/cache/staleness_test.go`.

### REQ-TTL-002 — Freshness verdict algorithm
**MUST.** Given corrected age `a` and effective TTL `t`:
- `t == 0` → `UNKNOWN` is not produced; the verdict is `STALE` by construction so
  that "always live" request types never take the fresh branch.
- `a` unavailable (no `observedAt`) → `UNKNOWN`.
- `a <= t` → `FRESH`.
- `a > t` → `STALE`.

**Verified by** `TestFreshness_VerdictTable` in `internal/freshness/evaluate_test.go`.

### REQ-TTL-003 — Effective TTL resolution order
**MUST.** Effective TTL = tenant override → request-type config → routing
default. Resolution MUST be logged once per request at debug level and exposed as
`meta.freshness.ttlSeconds`.

**Verified by** `TestTTL_TenantOverrideWins` in `internal/config/effective_test.go`.

### REQ-TTL-004 — Clock-skew correction
**MUST.** Age is computed as `age = (source.server_time - source.last_updated)`
when `server_time` is present, i.e. entirely in the source's own clock domain.
Only when `server_time` is absent does the BFF fall back to
`age = (bff_now - source.last_updated)`. In the fallback path, the estimated skew
`s = bff_now - source.server_time` is clamped to
`routing.defaults.clock_skew_tolerance` (default `2s`) and subtracted. A
computed negative age MUST be clamped to `0` and MUST raise warning
`CLOCK_SKEW_DETECTED`. Skew exceeding tolerance by more than 10× MUST force
verdict `UNKNOWN`.

*Rationale.* Comparing two clocks is the single most common source of false
"fresh" verdicts; staying inside one clock domain removes the comparison.

**Verified by** `TestFreshness_SameClockDomain`, `TestFreshness_SkewClamped`, `TestFreshness_NegativeAgeClamped`, `TestFreshness_GrossSkewForcesUnknown` in `internal/freshness/skew_test.go`.

### REQ-TTL-005 — Freshness probe is cheap and independently budgeted
**MUST.** The probe uses `sources.operational.freshness_probe_timeout`
(default `120ms`), which is strictly less than `call_timeout`. Probe failure MUST
NOT fail the request: it yields verdict `UNKNOWN`, which routes via rule 10.

**Verified by** `TestProbe_OwnTimeout`, `TestProbe_FailureYieldsUnknown` in `internal/freshness/probe_test.go`.

### REQ-TTL-006 — Probe results are memoized within a request and briefly across requests
**MUST.** A probe result is memoized for the life of the request. Across
requests it MAY be cached for `min(cache_ttl, 1s)` keyed by
`(tenant, resourceId)`; the memo MUST store `observedAt`, not a precomputed
verdict, so that the verdict is recomputed against the current clock.

*Rationale.* Caching the verdict rather than the observation makes cached data
appear to stop ageing.

**Verified by** `TestProbe_MemoStoresObservationNotVerdict` in `internal/freshness/probe_test.go`.

### REQ-TTL-007 — `on_unknown_freshness` is configurable
**MUST.** When the verdict is `UNKNOWN`, rule 10 routes to
`routing.defaults.on_unknown_freshness` ∈ `operational | execution | none`,
default `operational`. When it resolves to `none`, the request fails with
`NO_SOURCE_AVAILABLE` unless stale-serve applies.

**Verified by** `TestRouter_OnUnknownFreshnessConfigurable` in `internal/router/unknown_test.go`.

### REQ-TTL-008 — Stale-serve bound
**MUST.** Stale data MAY be served only when `allow_stale = true` **and**
`age <= max_stale`. Beyond `max_stale` the data MUST be discarded and the request
treated as if the source produced nothing.

*Rationale.* Unbounded stale-serve turns a cache into a silent archive of wrong
answers.

**Verified by** `TestStale_BoundedByMaxStale` in `internal/freshness/stale_test.go`.

### REQ-TTL-009 — Observed age is recorded as a distribution
**MUST.** Every served response records `data_freshness_age` (histogram,
seconds) attributed by `request_type`, `source`, `tenant_id`.

**Verified by** `TestFreshness_AgeHistogramRecorded` in `internal/freshness/observability_test.go`.

### REQ-TTL-010 — TTL of zero means always live
**MUST.** `ttl: 0s` (e.g. `execution_history`) MUST bypass freshness-based reuse
entirely: no fresh branch, no stale-serve, and `cache_ttl` MUST also be `0`.
Configuration with `ttl: 0` and `cache_ttl > 0` MUST fail validation at startup.

**Verified by** `TestConfig_ZeroTTLRejectsPositiveCacheTTL` in `internal/config/validate_test.go`.

---

## 5. REQ-PREC — Source precedence

### REQ-PREC-001 — Precedence is declared per field group, not per source
**MUST.** `precedence.fields` declares an ordered source list for each field
group. `internal/policy.SourcePrecedencePolicy` resolves each group
independently: the first source in the list that supplied a non-empty value wins.

*Rationale.* "The operational source wins" is false for execution history and
true for topology; precedence must be field-scoped.

**Verified by** `TestPrecedence_PerFieldGroup` in `internal/policy/precedence_test.go`.

### REQ-PREC-002 — Declared precedence from the contract
**MUST.** The default table is exactly:

| Field group | Order |
|---|---|
| `status` | operational, execution |
| `configuration` | operational |
| `metrics` | operational |
| `topology` | operational |
| `owner` | operational |
| `latestExecution` | execution |
| `executionHistory` | execution |
| `lastOperation` | execution |

**Verified by** `TestPrecedence_DefaultTableMatchesContract` in `internal/policy/precedence_test.go`.

### REQ-PREC-003 — Execution overrides while running
**MUST.** When the latest execution for the resource is in status `RUNNING` (or
the ODS record carries a non-empty `in_flight_execution_ref`), the field groups
listed in `precedence.execution_overrides_when_running` (default `[status,
subState]`) MUST take their value from the execution source regardless of
the base precedence order. A `CONFLICT_RESOLVED` warning is emitted only when
the two sources actually disagree.

*Rationale.* Mid-execution the operational record lags the workflow by design;
the execution source is authoritative about "what is happening right now".

**Verified by** `TestPrecedence_ExecutionOverridesWhenRunning`, `TestPrecedence_NoWarningWhenAgreeing` in `internal/policy/precedence_running_test.go`.

### REQ-PREC-004 — Conflict detection and counting
**MUST.** When two sources supply differing non-empty values for the same field
group, `precedence_conflict_total{field_group, winner, loser, tenant_id}` is
incremented and a `CONFLICT_RESOLVED` warning is added naming the losing
source. The response MUST still be produced.

**Verified by** `TestPrecedence_ConflictCounted` in `internal/policy/precedence_test.go`.

### REQ-PREC-005 — Empty is not a value
**MUST.** A zero-value, empty string, empty slice, empty map, or an enum decoded
to `UNKNOWN` MUST NOT displace a populated value from a lower-precedence source.
Explicit tombstones (a source signalling deletion) are out of scope for v1.

*Rationale.* Otherwise a partially populated higher-precedence record blanks out
a complete lower-precedence one (REQ-EDGE-006/007).

**Verified by** `TestPrecedence_EmptyDoesNotDisplace` in `internal/policy/precedence_test.go`.

### REQ-PREC-006 — Timestamp is not a tiebreaker by default
**MUST.** Precedence order, not record recency, decides conflicts. Recency MAY be
used only within a single field group where both candidates come from the same
`SourceKind`.

*Rationale.* The two sources' timestamps mean different things (last poll vs
workflow step completion); comparing them is a category error (REQ-EDGE-009).

**Verified by** `TestPrecedence_TimestampNotTiebreaker` in `internal/policy/precedence_test.go`.

### REQ-PREC-007 — Precedence is configurable per tenant
**MUST.** Tenants may override `precedence.fields` and
`execution_overrides_when_running`. Overrides MUST be validated at load: every
listed source must be `operational` or `execution`; every field group must exist
in the field catalog.

**Verified by** `TestPrecedence_TenantOverrideValidated` in `internal/config/validate_test.go`.

---

## 6. REQ-AGG — Aggregation

### REQ-AGG-001 — Fan-out is concurrent with independent deadlines
**MUST.** For `Target = BOTH`, both source calls start concurrently, each bounded
by its own `PerSourceTimeout` (REQ-RT-006), inside a parent context bounded by
the request deadline.

**Verified by** `TestAggregate_ConcurrentStart`, `TestAggregate_IndependentDeadlines` in `internal/aggregation/fanout_test.go`.

### REQ-AGG-002 — A failing optional source never cancels a healthy required one
**MUST.** Aggregation MUST NOT use a shared cancel-on-first-error group. Errors
are collected per source; the parent context is cancelled only on request
deadline or client disconnect.

*Rationale.* `errgroup.WithContext` semantics here would convert a benign optional
failure into a total outage.

**Verified by** `TestAggregate_OptionalFailureDoesNotCancelRequired` in `internal/aggregation/fanout_test.go`.

### REQ-AGG-003 — Result is a per-source outcome record
**MUST.** Aggregation yields, per source, one of `{value, err, timedOut,
skipped}` plus observed latency and observation time. The aggregate MUST NOT
collapse these into a single error.

**Verified by** `TestAggregate_PerSourceOutcome` in `internal/aggregation/result_test.go`.

### REQ-AGG-004 — Partial success rules
**MUST.** If all required sources succeeded, the response is `200` with
`partial = true` when any optional source failed. If any required source failed
and no stale/cached substitute is available, the response is `206` when at least
one field group is present, otherwise the error path (REQ-API-008).

**Verified by** `TestAggregate_PartialMatrix` in `internal/aggregation/partial_test.go`.

### REQ-AGG-005 — Goroutine hygiene
**MUST.** Every goroutine spawned by aggregation terminates before the handler
returns; no goroutine writes to the result after the deadline. Tests MUST assert
this with `goleak`.

**Verified by** `TestAggregate_NoGoroutineLeak` in `internal/aggregation/leak_test.go`.

### REQ-AGG-006 — Aggregation latency is measured as wall time of the fan-out
**MUST.** `aggregation_latency` records the span from first dispatch to last
completion (or deadline), not the sum of source latencies.

**Verified by** `TestAggregate_LatencyIsWallTime` in `internal/aggregation/observability_test.go`.

### REQ-AGG-007 — Bounded fan-out width
**MUST.** Fan-out width is at most the number of distinct sources in the
decision (≤ 2 in v1). Aggregation MUST NOT issue per-item N+1 calls; execution
enrichment for a resource uses the single `latest-execution` endpoint.

**Verified by** `TestAggregate_NoNPlusOne` in `internal/aggregation/fanout_test.go`.

---

## 7. REQ-MAP — Schema mapping

### REQ-MAP-001 — Canonical model is source-agnostic
**MUST.** `internal/domain` MUST NOT import `opsv1`, any REST DTO package, any
transport package, or `net/http`. Enforced by an import-graph test.

*Rationale.* The dependency rule is what keeps two heterogeneous schemas from
leaking into the API contract.

**Verified by** `TestDomain_NoOutboundImports` in `internal/domain/imports_test.go`.

### REQ-MAP-002 — Mapping is explicit and total over source enums
**MUST.** `OperationalMapper` and `ExecutionMapper` map every declared source
enum value to a canonical value. An unrecognized enum value maps to the canonical
`UNKNOWN` member and raises warning `SCHEMA_VERSION_MISMATCH`; it MUST NOT map to a
plausible-looking neighbour.

**Verified by** `TestMapper_EnumTotality_Operational`, `TestMapper_EnumTotality_Execution`, `TestMapper_UnknownEnumWarns` in `internal/mapper/enum_test.go`.

### REQ-MAP-003 — Operational enum mapping table
**MUST.** `ResourceState` → `domain.ResourceStatus`:

| ODS `ResourceState` | Canonical `ResourceStatus` |
|---|---|
| `RESOURCE_STATE_UNSPECIFIED` | `UNKNOWN` |
| `RESOURCE_STATE_PROVISIONING` | `PENDING` |
| `RESOURCE_STATE_ACTIVE` | `ACTIVE` |
| `RESOURCE_STATE_SUSPENDED` | `SUSPENDED` |
| `RESOURCE_STATE_DEGRADED` | `DEGRADED` |
| `RESOURCE_STATE_TERMINATING` | `TERMINATING` |
| `RESOURCE_STATE_TERMINATED` | `TERMINATED` |
| `RESOURCE_STATE_ERROR` | `ERROR` |

**Verified by** `TestMapper_ResourceStateTable` in `internal/mapper/operational_test.go`.

### REQ-MAP-004 — Execution enum mapping table
**MUST.** EDS `status` string → `domain.ExecutionStatus`, case-insensitive:
`queued|pending|scheduled → QUEUED`; `running|in_progress|executing → RUNNING`;
`completed|succeeded|success → COMPLETED`; `failed|error → FAILED`;
`cancelled|canceled|aborted → CANCELLED`; `timed_out|timeout → TIMED_OUT`;
anything else → `UNKNOWN` + `SCHEMA_VERSION_MISMATCH`.

**Verified by** `TestMapper_ExecutionStatusTable` in `internal/mapper/execution_test.go`.

### REQ-MAP-005 — Field renames are declared, not incidental
**MUST.** Every rename (`customer_ref → customerId`, `state → status`,
`substate → subState`, `ownership → owner`, `topology.upstream_refs →
topology.upstream`, …) is listed in `spec/data-contracts.md` and asserted by a
table test.

**Verified by** `TestMapper_FieldRenameTable` in `internal/mapper/operational_test.go`.

### REQ-MAP-006 — Unmapped source fields are dropped deliberately
**MUST.** Source fields with no canonical home (`operational_metadata`,
`refresh_source`, `cost_centre`, EDS `internalTraceId`) are dropped. The drop
list is enumerated in `spec/data-contracts.md`; a source field that is neither
mapped nor on the drop list MUST fail the mapper completeness test.

*Rationale.* Silent drops are how fields go missing for a year.

**Verified by** `TestMapper_CompletenessAgainstDropList` in `internal/mapper/completeness_test.go`.

### REQ-MAP-007 — Mappers are pure and side-effect free
**MUST.** Mappers take a source DTO plus a `MapContext` (tenant, clock, warning
sink) and return `(canonical, warnings, error)`. They MUST NOT perform I/O, read
global clocks, or log.

**Verified by** `TestMapper_Purity` in `internal/mapper/purity_test.go`.

### REQ-MAP-008 — Malformed source payloads are rejected, not coerced
**MUST.** Missing mandatory identity fields (`resourceId`, `tenantId`), a
timestamp that fails to parse, or a numeric field that is `NaN`/`Inf` MUST produce
`UPSTREAM_INVALID_RESPONSE`. Optional fields MAY be defaulted with a warning.

**Verified by** `TestMapper_RejectsMalformed`, `TestMapper_DefaultsOptional` in `internal/mapper/invalid_test.go`.

### REQ-MAP-009 — Numeric and unit fidelity
**MUST.** Metric values map as `float64` with the source unit carried verbatim
into `Metric.Unit`. The mapper MUST NOT convert units.

**Verified by** `TestMapper_UnitsPreserved` in `internal/mapper/operational_test.go`.

### REQ-MAP-010 — Timestamps normalize to UTC
**MUST.** All source timestamps convert to `time.Time` in UTC. A zero
`google.protobuf.Timestamp` maps to a zero `time.Time` and is treated as absent,
not as the Unix epoch.

**Verified by** `TestMapper_ZeroTimestampIsAbsent` in `internal/mapper/time_test.go`.

---

## 8. REQ-DS — Data sources

### REQ-DS-001 — Sources are behind ports owned by the application
**MUST.** `internal/datasource` declares `OperationalPort` and `ExecutionPort`
interfaces in terms of canonical/domain types and adapter-neutral parameters.
Adapters (`operational/grpc`, `execution/rest`) implement them. The application
layer MUST depend only on the ports.

**Verified by** `TestPorts_ApplicationDependsOnInterfaces` in `internal/application/imports_test.go`.

### REQ-DS-002 — ODS exposes a cheap freshness probe
**MUST.** `OperationalService.GetResourceFreshness` returns only the
`FreshnessEnvelope` and a `found` flag. The adapter MUST expose it as
`OperationalPort.Freshness(ctx, tenant, resourceID)` and MUST NOT synthesise it
from a full read.

**Verified by** `TestOperationalAdapter_FreshnessUsesProbeRPC` in `internal/datasource/operational/adapter_test.go`.

### REQ-DS-003 — ODS adapter surface
**MUST.** The adapter wraps `GetResource`, `GetResourceState`,
`BatchGetResources`, `GetResourceFreshness`, `Health`. Every call populates
`RequestContext{tenant_id, correlation_id, principal}`.

**Verified by** `TestOperationalAdapter_PopulatesRequestContext` in `internal/datasource/operational/adapter_test.go`.

### REQ-DS-004 — EDS adapter surface
**MUST.** The REST adapter implements exactly:
`GET /eds/v1/executions?resourceId=&tenantId=&limit=`,
`GET /eds/v1/executions/{executionId}`,
`GET /eds/v1/resources/{resourceId}/latest-execution`,
`GET /eds/v1/health`.

**Verified by** `TestExecutionAdapter_EndpointSet` in `internal/datasource/execution/adapter_test.go`.

### REQ-DS-005 — Connection management
**MUST.** The gRPC client uses a single long-lived `ClientConn` with
`max_conns` sub-connections, keepalive
(`time`, `timeout`, `permit_without_stream`) from config, and
`WaitForReady(false)` so that a down backend fails fast rather than queuing to
the deadline. The HTTP client uses a shared transport with bounded
`MaxIdleConnsPerHost` and explicit `IdleConnTimeout`.

*Rationale.* `WaitForReady(true)` converts an unavailable backend into a timeout,
which the breaker misclassifies.

**Verified by** `TestOperationalAdapter_FailFastWhenUnavailable`, `TestExecutionAdapter_TransportPooling` in `internal/datasource/operational/conn_test.go`, `internal/datasource/execution/conn_test.go`.

### REQ-DS-006 — Health is polled, not probed inline
**MUST.** Source health is maintained by a background poller
(default 5 s interval, jittered) and read from an in-memory snapshot by the
router. The router MUST NOT issue a health RPC on the request path.

**Verified by** `TestHealthPoller_SnapshotReadIsNonBlocking` in `internal/datasource/health_test.go`.

### REQ-DS-007 — Adapters translate transport errors into `pkg/errs` types
**MUST.** gRPC codes and HTTP statuses map to the typed taxonomy before crossing
the port boundary; no `status.Status` or `*http.Response` escapes an adapter.

**Verified by** `TestAdapters_ErrorTranslation` in `internal/datasource/errors_test.go`.

### REQ-DS-008 — All reads are idempotent and safely retryable
**MUST.** Every port method is a read. Retry (REQ-RES-002) is permitted for all
of them subject to the retryability classification.

**Verified by** `TestPorts_AllMethodsAreReads` in `internal/datasource/ports_test.go`.

### REQ-DS-009 — Response size limits
**MUST.** gRPC `MaxCallRecvMsgSize` and the REST body reader are bounded
(default 4 MiB). An oversize source response yields
`UPSTREAM_INVALID_RESPONSE`, not an OOM.

**Verified by** `TestAdapters_ResponseSizeBounded` in `internal/datasource/limits_test.go`.

### REQ-DS-010 — Reference stubs implement the same contracts
**MUST.** `cmd/opsource` and `cmd/exsource` satisfy the identical contracts and
expose chaos controls on `:9111` / `:9112` (latency, error rate, staleness
offset, partial payload, invalid payload, schema version).

**Verified by** `TestContract_OpSourceStub`, `TestContract_ExSourceStub` in `test/contract/opsource_test.go`, `test/contract/exsource_test.go`.

---

## 9. REQ-CACHE — Caching

### REQ-CACHE-001 — Two-layer cache-aside
**MUST.** Reads consult L1 (in-process, `cache.l1.max_entries`, `cache.l1.ttl`),
then L2 (Redis), then the source. Writes populate both. L2 failure MUST degrade
to L1-only, never fail the request.

**Verified by** `TestCache_LayerOrder`, `TestCache_L2FailureDegrades` in `internal/cache/cache_test.go`.

### REQ-CACHE-002 — Key structure includes every isolation dimension
**MUST.** Key = `{key_prefix}:v1:{tenantId}:{requestType}:{resourceId}[:{executionId}][:{paramHash}]`.
`tenantId` MUST be a structural component, never a suffix or a value inside the
payload.

*Rationale.* Tenant leakage via cache keys is the highest-severity multi-tenancy
failure mode (REQ-EDGE-016).

**Verified by** `TestCacheKey_Structure`, `TestCacheKey_TenantSeparation` in `internal/cache/key_test.go`.

### REQ-CACHE-003 — Cached entries carry freshness provenance
**MUST.** An entry stores `{payload, observedAt, sourceKind, schemaVersion,
storedAt}`. On hit, freshness is re-evaluated against `observedAt` (REQ-TTL-001),
and `meta.cache = {hit: true, layer: "L1"|"L2"}` is set.

**Verified by** `TestCache_EntryCarriesObservedAt` in `internal/cache/entry_test.go`.

### REQ-CACHE-004 — Stampede protection
**MUST.** Concurrent misses for the same key collapse via `singleflight`
in-process; across replicas, `cache.stampede.lock_ttl` guards L2 fill. Waiters
MUST observe the same result and MUST NOT each call the source.

**Verified by** `TestCache_SingleflightCollapses`, `TestCache_ConcurrentSameResource` in `internal/cache/singleflight_test.go`.

### REQ-CACHE-005 — Negative caching
**MUST.** A confirmed `NOT_FOUND` is cached for `cache.negative_ttl`
(default `5s`) and served as `404` without contacting sources. Errors other than
`NOT_FOUND` MUST NOT be negatively cached.

**Verified by** `TestCache_NegativeCaching`, `TestCache_ErrorsNotNegativelyCached` in `internal/cache/negative_test.go`.

### REQ-CACHE-006 — Cache is bypassed for STRONG consistency
**MUST.** `Consistency = STRONG` (REQ-CLS-004) skips both the cache read and
stale-serve; the response MUST still populate the cache on success.

Implemented in `application.Service.load`, which calls the loader directly rather
than `Manager.GetOrLoad` and then writes through `Manager.Store`. The observable
consequence is that `meta.cache.hit` is always `false` for a strongly-consistent
request — `execution_status` is configured `consistency: strong`, so
`/executions/{executionId}` is never *answered* from cache despite its
`cache_ttl: 2s`.

**Verified by** `TestCache_StrongBypassesRead` in `internal/cache/consistency_test.go`.

### REQ-CACHE-007 — Serialization is versioned
**MUST.** Entries carry a `schemaVersion`. On mismatch the entry is treated as a
miss and evicted; it MUST NOT be decoded into a struct of a different shape.

**Verified by** `TestCache_SchemaVersionMismatchEvicts` in `internal/cache/entry_test.go`.

### REQ-CACHE-008 — Bounded memory
**MUST.** L1 is size-bounded with LRU eviction and MUST NOT hold references that
prevent GC of evicted payloads. Entry count is exported for capacity planning.

**Verified by** `TestCache_L1EvictionBounded` in `internal/cache/l1_test.go`.

### REQ-CACHE-009 — Cache never invents freshness
**MUST.** A cache hit MUST NOT set `meta.freshness.state = FRESH` on its own
authority; the verdict derives from the stored `observedAt` only. A hit whose
data is beyond TTL is served (if stale-serve allows) with `degraded = true` and
warning `STALE_DATA`.

**Verified by** `TestCache_HitCanStillBeStale` in `internal/cache/staleness_test.go`.

### REQ-CACHE-010 — Cache metrics
**MUST.** `cache_hit_total{layer}` and `cache_miss_total{layer}` are emitted with
`tenant_id`, `request_type`.

**Verified by** `TestCache_MetricsEmitted` in `internal/cache/observability_test.go`.

---

## 10. REQ-RES — Resilience

### REQ-RES-001 — Every outbound call is deadline-bounded
**MUST.** No outbound call may use `context.Background()`. The request deadline
is derived from `server.write_timeout` minus a response-budget reserve, and each
source call is further bounded by `PerSourceTimeout`.

**Verified by** `TestResilience_NoUnboundedContext` (static analysis test over the package set) in `internal/resilience/deadline_test.go`.

### REQ-RES-002 — Bounded retry with exponential backoff and full jitter
**MUST.** Retries are limited to `retry.max_attempts` (total attempts, default 3),
with delay `rand(0, min(max_backoff, base_backoff * 2^(n-1)))` — full jitter. The
cumulative retry budget MUST fit inside the remaining request deadline; a retry
that cannot complete before the deadline MUST NOT be attempted.

*Rationale.* Equal-jitter and no-jitter schedules resynchronize clients after an
outage and cause a thundering herd on recovery.

**Verified by** `TestRetry_FullJitterDistribution`, `TestRetry_RespectsDeadline`, `TestRetry_MaxAttempts` in `internal/resilience/retry_test.go`.

### REQ-RES-003 — Never retry blindly
**MUST.** Retry eligibility is decided by the typed error classification
(REQ-RES-004), not by "an error occurred". Terminal errors (`INVALID_REQUEST`,
`UNAUTHENTICATED`, `FORBIDDEN`, `NOT_FOUND`, `TENANT_MISMATCH`,
`SCHEMA_VERSION_MISMATCH`, `UPSTREAM_INVALID_RESPONSE`) MUST NOT be retried.
`RATE_LIMITED` is retried only when the upstream supplies `Retry-After` that fits
the deadline.

**Verified by** `TestRetry_EligibilityTable` in `internal/resilience/retry_eligibility_test.go`.

### REQ-RES-004 — Error classification is three-valued
**MUST.** Every error carries `Retryable`, `Terminal`, `Degradable`. `Degradable`
means "this failure may be answered with stale or partial data".

**Verified by** `TestErrs_ClassificationTable` in `pkg/errs/classify_test.go`.

### REQ-RES-005 — Circuit breaker per source
**MUST.** A breaker per `(source, tenant-class)` transitions
`CLOSED → OPEN` on a rolling failure ratio over a minimum request volume,
`OPEN → HALF_OPEN` after a cooldown, and `HALF_OPEN → CLOSED` after N consecutive
successes with a bounded probe concurrency of 1. Transitions emit
`circuit_breaker_transition_total` and update `circuit_breaker_state`.

**Verified by** `TestBreaker_StateMachine`, `TestBreaker_HalfOpenSingleProbe`, `TestBreaker_MetricsOnTransition` in `internal/resilience/breaker_test.go`.

### REQ-RES-006 — Open breaker fails fast and is routable
**MUST.** An open breaker returns `UPSTREAM_UNAVAILABLE` with source state
`CIRCUIT_OPEN` immediately, and the health snapshot reports the source as
unavailable so routing rule 7 can pick the fallback *before* dispatch.

**Verified by** `TestBreaker_OpenFeedsHealthSnapshot` in `internal/resilience/breaker_health_test.go`.

### REQ-RES-007 — Bulkhead per source
**MUST.** Concurrency to each source is capped by
`bulkhead.max_concurrent` with `bulkhead.acquire_timeout`. Acquire failure is
`UPSTREAM_UNAVAILABLE` (degradable). `bulkhead_in_flight` is exported.

*Rationale.* A slow EDS must not consume all goroutines and starve ODS traffic.

**Verified by** `TestBulkhead_CapsConcurrency`, `TestBulkhead_AcquireTimeout` in `internal/resilience/bulkhead_test.go`.

### REQ-RES-008 — Inbound rate limiting
**MUST.** Token-bucket limiting at `security.rate_limit.rps`/`burst`, keyed per
tenant when `per_tenant: true`, else globally. Rejection is `429 RATE_LIMITED`
with `Retry-After`.

**Verified by** `TestRateLimit_PerTenantBucket`, `TestRateLimit_RetryAfterHeader` in `internal/resilience/ratelimit_test.go`.

### REQ-RES-009 — Graceful degradation ladder
**MUST.** On primary failure the order is: (1) fallback source per decision,
(2) fresh cache, (3) stale cache within `max_stale`, (4) partial response,
(5) error. Each step taken is recorded as a warning and reflected in
`degraded`/`partial`.

**Verified by** `TestDegradation_Ladder` in `internal/application/degradation_test.go`.

### REQ-RES-010 — Graceful shutdown
**MUST.** On `SIGTERM`, readiness flips to `503` immediately, in-flight requests
drain for `server.shutdown_grace`, then listeners close. No in-flight request is
severed before the grace period expires.

**Verified by** `TestShutdown_DrainsInFlight` in `internal/api/shutdown_test.go`.

### REQ-RES-011 — Client disconnect cancels upstream work
**MUST.** Client cancellation propagates to source calls; a cancelled request
MUST NOT count as an upstream failure for breaker purposes.

*Rationale.* Otherwise client-side timeouts trip breakers and cause a
self-inflicted outage.

**Verified by** `TestCancellation_NotCountedByBreaker` in `internal/resilience/cancel_test.go`.

### REQ-RES-012 — Panics are contained
**MUST.** A panic in a handler or a fan-out goroutine is recovered, logged with
stack, counted, and returned as `500 INTERNAL`; it MUST NOT crash the process.

**Verified by** `TestRecovery_HandlerPanic`, `TestRecovery_GoroutinePanic` in `internal/api/middleware/recovery_test.go`.

---

## 11. REQ-OBS — Observability

### REQ-OBS-001 — Metric set is exactly the contract's
**MUST.** Counters `bff_request_total`, `operational_ttl_hit_total`,
`operational_ttl_miss_total`, `execution_fallback_total`, `datasource_error_total`,
`cache_hit_total`, `cache_miss_total`, `partial_response_total`,
`stale_response_total`, `routing_decision_total`,
`circuit_breaker_transition_total`, `precedence_conflict_total`. Histograms
(seconds) `bff_request_latency`, `operational_source_latency`,
`execution_source_latency`, `aggregation_latency`, `data_freshness_age`. Gauges
`bff_concurrent_requests`, `bulkhead_in_flight`, `circuit_breaker_state`.

**Verified by** `TestMetrics_InstrumentInventory` in `internal/observability/metrics_test.go`.

### REQ-OBS-002 — Attribute set is bounded
**MUST.** Permitted attributes: `tenant_id`, `request_type`, `routing_decision`,
`routing_rule`, `source`, `outcome`, `http_status`, `degraded`, `partial`,
plus `layer` (cache), `field_group`/`winner`/`loser` (precedence), `state`
(breaker). `resourceId`, `executionId`, `correlationId` and any user-supplied
free text MUST NOT be metric attributes.

*Rationale.* Unbounded label cardinality is the dominant cost and outage risk in
metrics backends.

**Verified by** `TestMetrics_NoHighCardinalityAttributes` in `internal/observability/cardinality_test.go`.

### REQ-OBS-003 — Span tree
**MUST.** The server span is named by the matched **route pattern** (e.g.
`GET /api/v1/resources/{resourceId}/status`), never by the raw path, so trace
cardinality stays bounded the way metric attributes do; an unmatched request is
`<METHOD> unmatched`. Beneath it the service emits exactly five spans:

```
<route pattern>
└── bff.usecase.resource | bff.usecase.execution
    ├── bff.route                    routing_target, routing_rule, freshness
    ├── bff.aggregate                routing_target, routing_rule
    └── bff.resolve_in_flight        only when the ODS record declared an
                                     in_flight_execution_ref; execution_id
```

There is deliberately **no** span per source call, per mapper invocation, per
cache lookup or per precedence decision: those are metrics
(`operational_source_latency`, `execution_source_latency`, `cache_hit_total`,
`precedence_conflict_total`). A read path this hot should not pay for span
construction on every internal step, and the interesting questions — which rule
fired, what the verdict was, how long the fan-out took — are answerable from
these five spans plus the response envelope.

**Verified by** `TestTracing_SpanTreeShape` in `internal/observability/span_test.go`.

### REQ-OBS-004 — Errors recorded on spans
**MUST.** Failing spans set status `Error`, record the exception, and carry
`error.code`. A degraded-but-successful request sets span status `Ok` with
`bff.degraded = true`.

**Verified by** `TestTracing_ErrorRecording` in `internal/observability/span_test.go`.

### REQ-OBS-005 — Structured JSON logs
**MUST.** `slog` JSON with fields `ts`, `level`, `msg`, `service`, `env`,
`correlation_id`, `trace_id`, `span_id`, `tenant_id`, `request_type`,
`routing_decision`, `routing_rule`, `http_status`, `duration_ms`, `degraded`,
`partial`, `sources`, `error_code`. One request-completion line per request.

**Verified by** `TestLogging_RequestLineSchema` in `internal/observability/log_test.go`.

### REQ-OBS-006 — Logs are redacted
**MUST.** Authorization headers, JWT contents, `password`/`secret`/`token`-named
config values, and configured PII field paths MUST never appear in logs. Enforced
by a redaction pass plus a test scanning emitted lines.

**Verified by** `TestLogging_RedactsSecrets` in `internal/security/redaction_test.go`.

### REQ-OBS-007 — Exemplars link metrics to traces
**SHOULD.** Latency histograms attach trace exemplars when the span is sampled.

**Verified by** `TestMetrics_ExemplarsAttached` in `internal/observability/exemplar_test.go`.

### REQ-OBS-008 — TTL hit/miss accounting is unambiguous
**MUST.** `operational_ttl_hit_total` increments only when the freshness verdict
was `FRESH` and the operational source was used as a result;
`operational_ttl_miss_total` increments on `STALE` or `UNKNOWN`. These count
freshness verdicts, not cache outcomes.

**Verified by** `TestMetrics_TTLHitMissSemantics` in `internal/observability/metrics_test.go`.

### REQ-OBS-009 — Telemetry export is non-blocking and failure-tolerant
**MUST.** OTLP export uses batching; exporter failure MUST NOT block or fail
request processing, and MUST be rate-limited in the logs.

**Verified by** `TestObservability_ExporterFailureIsolated` in `internal/observability/provider_test.go`.

### REQ-OBS-010 — Sampling is configurable and parent-respecting
**MUST.** `observability.trace_sample_ratio` drives a parent-based,
ratio-sampled sampler. Error and degraded requests SHOULD be sampled at a raised
rate.

**Verified by** `TestTracing_SamplerConfiguration` in `internal/observability/sampler_test.go`.

---

## 12. REQ-SEC — Security

### REQ-SEC-001 — Every data-plane request is authenticated
**MUST.** `Authorization: Bearer <jwt>` is required on all `/api/v1/*` routes.
Missing/malformed → `401 UNAUTHENTICATED`. Admin endpoints are exempt but are not
exposed on the data-plane listener (REQ-API-002).

**Verified by** `TestAuth_RequiredOnDataPlane` in `internal/api/middleware/auth_test.go`.

### REQ-SEC-002 — JWT validation is complete
**MUST.** Validate signature (JWKS via `security.jwt.jwks_url`, or HS256 from
`hs256_secret_env` in dev only), `iss`, `aud`, `exp`, `nbf`, `iat`, with
`security.jwt.leeway` (default `30s`) applied symmetrically. `alg` MUST be
allow-listed; `none` and algorithm confusion MUST be rejected. All
`required_claims` MUST be present.

**Verified by** `TestJWT_ValidationMatrix`, `TestJWT_RejectsAlgNone`, `TestJWT_LeewayApplied` in `internal/security/jwt_test.go`.

### REQ-SEC-003 — JWKS rotation without restart
**MUST.** JWKS is cached with TTL and refreshed on unknown `kid` (rate-limited,
with negative caching to prevent a refresh storm from forged `kid`s). Fetch
failure falls back to the last good key set until its hard expiry.

**Verified by** `TestJWKS_RotationOnUnknownKid`, `TestJWKS_RefreshRateLimited` in `internal/security/jwks_test.go`.

### REQ-SEC-004 — Tenant is resolved from the token and enforced everywhere
**MUST.** Tenant identity comes from the JWT claim (`tenant_id`, configurable).
If `X-Tenant-ID` is present it MUST equal the claim, else `403 TENANT_MISMATCH`.
The resolved tenant is placed in the request context by
`pkg/correlation` and is a required parameter at every enforcement point:
cache key (REQ-CACHE-002), source call arguments, log/metric attributes.
Defence in depth: the sources also enforce isolation, and the BFF MUST NOT rely
on that alone.

**Verified by** `TestTenant_ClaimIsAuthoritative`, `TestTenant_HeaderMismatchRejected`, `TestTenant_EnforcementPoints` in `internal/security/tenant_test.go`.

### REQ-SEC-005 — RBAC per endpoint
**MUST.** Roles from `security.rbac.roles` map to permissions; each endpoint
declares a required permission. Missing permission → `403 FORBIDDEN`. Matrix in
`spec/security.md`.

**Verified by** `TestRBAC_EndpointPermissionMatrix` in `internal/security/rbac_test.go`.

### REQ-SEC-006 — Cross-tenant reads are impossible by construction
**MUST.** A response MUST NOT contain a record whose `tenantId` differs from the
authenticated tenant. Mappers assert this and return `TENANT_MISMATCH` on
violation.

*Rationale.* Belt and braces against a misbehaving or compromised source.

**Verified by** `TestTenant_ResponseTenantAsserted` in `internal/mapper/tenant_test.go`.

### REQ-SEC-007 — Transport security to sources
**MUST.** TLS is required for both sources outside local development; mTLS is
supported via configured client certificates. Certificate verification MUST NOT
be disableable by environment variable in production builds.

**Verified by** `TestTLS_ConfigEnforced`, `TestTLS_InsecureRejectedInProd` in `internal/config/tls_test.go`.

### REQ-SEC-008 — Secrets come from the environment, never from files in the image
**MUST.** Redis password and HS256 secret are read from env var names given in
config (`password_env`, `hs256_secret_env`). Secret *values* MUST NOT appear in
config files, logs, metrics, traces or error bodies.

**Verified by** `TestConfig_SecretsFromEnvOnly` in `internal/config/secrets_test.go`.

### REQ-SEC-009 — Input validation
**MUST.** `resourceId` and `executionId` match `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`;
`limit` is an integer in range; `cursor` is base64url ≤ 512 bytes. Violations →
`400 INVALID_REQUEST` before any source call.

**Verified by** `TestValidation_PathParams`, `TestValidation_RejectsBeforeUpstream` in `internal/api/middleware/validation_test.go`.

### REQ-SEC-010 — Request size limits
**MUST.** Request bodies are capped at `server.max_body_bytes`; header size is
capped by the HTTP server. Exceeding → `413 REQUEST_TOO_LARGE`.

**Verified by** `TestValidation_BodySizeLimit` in `internal/api/middleware/validation_test.go`.

### REQ-SEC-011 — Output filtering
**MUST.** Fields flagged sensitive in the field catalog (owner email, cost
centre, configuration keys matching secret patterns) are redacted or omitted
based on the caller's permissions.

**Verified by** `TestRedaction_OutputFiltering` in `internal/security/redaction_test.go`.

### REQ-SEC-012 — Audit logging
**MUST.** Emit audit events for: authentication failure, authorization denial,
tenant mismatch, rate-limit rejection, cross-tenant assertion failure,
configuration reload. Audit records carry `who` (subject), `what`, `when`,
`tenant_id`, `correlation_id`, and outcome.

**Verified by** `TestAudit_EventsEmitted` in `internal/security/audit_test.go`.

### REQ-SEC-013 — Error bodies do not leak internals
**MUST.** `detail` MUST NOT contain stack traces, upstream hostnames, SQL, raw
upstream payloads, or JWT contents. Internal detail goes to logs keyed by
`correlationId`.

**Verified by** `TestErrors_NoInternalLeakage` in `internal/api/response/error_test.go`.

### REQ-SEC-014 — Security headers
**SHOULD.** Responses carry `X-Content-Type-Options: nosniff`,
`Cache-Control: no-store` for authenticated responses, and no `Server` banner.

**Verified by** `TestHeaders_SecurityDefaults` in `internal/api/middleware/headers_test.go`.

---

## 13. REQ-MT — Multi-tenancy

### REQ-MT-001 — Tenant is a first-class request dimension
**MUST.** Tenant is present in the context from authentication until response
encoding, and is a mandatory argument to cache, router, adapters, and telemetry.
Code paths that accept a request without a tenant MUST fail closed
(rule `guard.tenant_missing`).

**Verified by** `TestTenant_MissingFailsClosed` in `internal/router/guard_test.go`.

### REQ-MT-002 — Per-tenant configuration overlay
**MUST.** `tenants.<id>` may override `routing`, `cache`, `security` subtrees.
Resolution is deep-merge over defaults; unknown tenants use defaults without
error. Overrides are validated with the same schema as defaults.

**Verified by** `TestConfig_TenantOverlayMerge`, `TestConfig_UnknownTenantUsesDefaults` in `internal/config/tenant_test.go`.

### REQ-MT-003 — Per-tenant resource isolation
**MUST.** Rate limits and bulkheads MAY be partitioned per tenant so that one
tenant cannot exhaust the shared budget. When `rate_limit.per_tenant` is true,
each tenant has an independent bucket.

**Verified by** `TestRateLimit_PerTenantBucket` in `internal/resilience/ratelimit_test.go`.

### REQ-MT-004 — Telemetry is tenant-attributed but not tenant-explosive
**MUST.** `tenant_id` is an attribute on all metrics; when configured tenant
count exceeds `observability.max_tenant_cardinality`, unknown tenants collapse to
`_other`.

**Verified by** `TestMetrics_TenantCardinalityCollapse` in `internal/observability/cardinality_test.go`.

### REQ-MT-005 — Cross-tenant cache poisoning is impossible
**MUST.** A cache entry written under tenant A MUST NOT be readable under tenant
B under any key-collision scenario, including hash collision of `paramHash`
(tenant is a distinct key segment, not part of the hash).

**Verified by** `TestCacheKey_TenantSeparation`, `TestCache_NoCrossTenantRead` in `internal/cache/key_test.go`.

---

## 14. REQ-CFG — Configuration

### REQ-CFG-001 — No TTL is compiled into Go source
**MUST.** Every TTL, timeout, backoff, breaker threshold, bulkhead limit, cache
size and rate limit is read from configuration. A literal `time.Duration`
constant used as a policy value in non-test source is a defect.

*Rationale.* Tuning must be a config change, not a release.

**Verified by** `TestConfig_NoHardcodedDurations` (AST scan of `internal/**` excluding `_test.go` and `internal/config/defaults.go`) in `internal/config/nohardcode_test.go`.

### REQ-CFG-002 — Layered resolution order
**MUST.** File (`configs/bff.yaml`) → environment (`BFF_` prefix, `__` nesting
separator) → tenant overlay. Later layers win. Example:
`BFF_SOURCES__OPERATIONAL__ADDR`,
`BFF_ROUTING__REQUEST_TYPES__RESOURCE_STATUS__TTL`.

**Verified by** `TestConfig_LayerPrecedence`, `TestConfig_EnvNestingSeparator` in `internal/config/layer_test.go`.

### REQ-CFG-003 — Startup validation is fail-fast and total
**MUST.** The process MUST NOT start with invalid configuration. Validation
covers: duration parseability and sign, `ttl: 0 ⇒ cache_ttl: 0` (REQ-TTL-010),
`freshness_probe_timeout < call_timeout`, `max_backoff >= base_backoff`,
`preferred_source`/`fallback` in the allowed enum, precedence field names in the
catalog, and required-source maps referring to real sources.

**Verified by** `TestConfig_ValidationMatrix` in `internal/config/validate_test.go`.

### REQ-CFG-004 — Hot reload is atomic
**MUST.** Reload (SIGHUP or file watch) swaps an immutable config snapshot behind
an atomic pointer. An in-flight request MUST see one consistent snapshot for its
whole lifetime. Invalid reload leaves the previous snapshot in place and logs an
audit event.

**Verified by** `TestConfig_ReloadAtomicity`, `TestConfig_InvalidReloadKeepsPrevious` in `internal/config/reload_test.go`.

### REQ-CFG-005 — Non-reloadable keys are declared
**MUST.** Listener addresses, TLS material paths and cache backend selection are
not hot-reloadable; a change is logged and requires restart. The set is
documented and asserted.

**Verified by** `TestConfig_NonReloadableKeys` in `internal/config/reload_test.go`.

### REQ-CFG-006 — Effective configuration is introspectable
**SHOULD.** The admin listener exposes `GET /config/routing?tenant=<t>` returning
the effective, redacted routing policy including per-tenant resolution, plus
`reload_count` and `reload_failures`.

**Verified by** `TestAdmin_ConfigEndpointRedacted` in `internal/api/handler/admin_test.go`.

### REQ-CFG-008 — Keys that govern behaviour documented elsewhere
**MUST.** Three keys are easy to overlook when reading the routing table alone,
and each governs a behaviour specified in another requirement. A configuration
reference that omits them is incomplete.

| Key | Default | Governs |
|---|---|---|
| `routing.defaults.resolve_in_flight_execution` | `true` | An operational-only read consults the execution source when the operational record declares an `in_flight_execution_ref`, so `/status` and `/details` cannot report different statuses for the same resource mid-workflow (REQ-PREC-003). The extra call is made only in the in-flight case, so the common case pays nothing, and it is best-effort: a failure leaves the operational answer standing. Note that fetching the execution is not the same as granting it authority — the precedence override still requires a candidate that is actually in progress. |
| `routing.defaults.in_flight_lookup_timeout` | `300ms` | The budget for that extra call. Deliberately much tighter than a full execution read, because it is an optimisation on a latency-sensitive endpoint rather than a required fetch. |
| `cache.stale_grace` | `5m` | How long an entry is **physically** retained past its logical `cache_ttl`, so it remains reachable by the stale-serve rung when every source is down (REQ-RES-007, REQ-EDGE-005). The logical lifetime is what `Manager.Get` enforces and what the envelope reports; only `Manager.GetStale` can see the physical one, which is why no ordinary read path can reach expired data by accident. |

Also worth stating because it is a cache TTL that reads like a freshness TTL:
`routing.defaults.probe_cache_ttl` (`1s`) bounds how often the BFF re-asks the
source "how old is this?". It memoises the *observation*, never the verdict, and
`freshness.Manager` advances the memoised `SourceTime` by the time the entry has
spent in the memo — so reusing a probe result yields a larger age, never a
fresher one (REQ-TTL-007).

`allow_stale`, `probe_enabled` and `resolve_in_flight_execution` are `*bool`
rather than `bool`, read through the accessors `StaleAllowed()`,
`ProbesEnabled()` and `InFlightResolutionEnabled()`, all of which default to
`true` when the pointer is nil. That is what lets a tenant overlay turn one of
them **off on its own**: with a plain `bool` an unset field is indistinguishable
from an explicit `false`, so the overlay had to be gated on a companion duration
being present, and a tenant block containing only `resolve_in_flight_execution:
false` was silently ignored. The YAML syntax is unchanged.

**Verified by** `TestConfig_Defaults` in `internal/config/config_test.go`,
`TestManager_ProbeMemoAges` in `internal/freshness/freshness_test.go`,
`TestConfig_TenantMayDisableSingleFlag` in `internal/config/config_test.go`.

### REQ-CFG-007 — Defaults are complete
**MUST.** The service starts with no config file present, using documented
defaults, in a mode where sources are unreachable and readiness is `503` — it
MUST NOT panic.

**Verified by** `TestConfig_StartsWithDefaults` in `internal/config/defaults_test.go`.

---

## 15. REQ-PERF — Performance

### REQ-PERF-001 — End-to-end latency budget
**MUST.** With healthy sources at nominal load:

| Route class | P95 | P99 |
|---|---|---|
| Cache hit (any) | 5 ms | 15 ms |
| `OPERATIONAL` only | 60 ms | 120 ms |
| `EXECUTION` only | 250 ms | 500 ms |
| `BOTH` (fan-out) | 280 ms | 550 ms |

**Verified by** `TestPerf_LatencyBudgets` in `test/load/thresholds_test.go`, enforced by `test/load/k6/*.js` thresholds.

### REQ-PERF-002 — Freshness probe budget
**MUST.** The probe adds ≤ 15 ms at P95 and ≤ `freshness_probe_timeout` in the
worst case. Probe cost MUST be strictly less than the cost of the full read it
avoids; a probe that is not cheaper is a design failure.

**Verified by** `TestPerf_ProbeCheaperThanRead` in `test/integration/probe_budget_test.go`.

### REQ-PERF-003 — BFF overhead
**MUST.** BFF-attributable time (total minus max source latency for the request)
is ≤ 12 ms at P99 for `BOTH`, ≤ 6 ms at P99 otherwise.

**Verified by** `TestPerf_BFFOverhead` in `test/load/thresholds_test.go`.

### REQ-PERF-004 — Allocation discipline on the hot path
**SHOULD.** Steady-state per request: ≤ 120 allocations and ≤ 48 KiB for a cache
hit. Benchmarks with `-benchmem` gate regressions in CI.

**Verified by** `BenchmarkResponseBuild_CacheHit` in `internal/api/response/bench_test.go`.

### REQ-PERF-005 — Throughput and saturation behaviour
**MUST.** At 2× the configured bulkhead capacity the service sheds load via
bulkhead/rate limit with bounded latency; queue depth MUST NOT grow without
bound and P99 MUST NOT exceed 2× the healthy budget.

**Verified by** `TestPerf_SaturationShedsLoad` in `test/load/saturation_test.go`.

### REQ-PERF-006 — Fan-out costs the slower source, not the sum
**MUST.** `BOTH` latency ≈ max(operational, execution) + overhead, never their
sum; asserted by comparing measured `aggregation_latency` to per-source
histograms.

**Verified by** `TestPerf_FanOutIsMaxNotSum` in `internal/aggregation/latency_test.go`.

---

## 16. REQ-EDGE — The twenty mandated edge cases

Each edge case is a required, independently testable behaviour. `test/integration`
drives them against the reference stubs using the chaos knobs (REQ-DS-010).

### REQ-EDGE-001 — Operational data fresh
**MUST.** `age <= ttl` and ODS healthy ⇒ rule `ttl.operational.fresh`, target
`OPERATIONAL`, EDS not called, `freshness.state = FRESH`, `degraded = false`,
`operational_ttl_hit_total` incremented.
*Rationale.* The optimal path must be provably free of EDS cost.
**Verified by** `TestEdge001_OperationalFresh` in `test/integration/edge_freshness_test.go`.

### REQ-EDGE-002 — Operational data stale
**MUST.** `age > ttl` ⇒ rule `ttl.operational.stale`. With a configured fallback
and `allow_stale=false`, target is `EXECUTION` and `execution_fallback_total`
increments. With `allow_stale=true` and `age <= max_stale`, target stays
`OPERATIONAL` with `degraded = true`, `freshness.state = STALE` and warning
`STALE_DATA`. Beyond `max_stale` the operational data is discarded.
**Verified by** `TestEdge002_OperationalStale_FallsBack`, `TestEdge002_OperationalStale_ServesStale`, `TestEdge002_BeyondMaxStale` in `test/integration/edge_freshness_test.go`.

### REQ-EDGE-003 — Operational source unavailable
**MUST.** ODS down/breaker open ⇒ rule `health.primary_unavailable`, target =
configured fallback. If the request type has `fallback: none`, degrade to cache
(fresh, then stale within `max_stale`), else `503 NO_SOURCE_AVAILABLE` with
`sources: [{OPERATIONAL, CIRCUIT_OPEN|UNAVAILABLE}]`. The ODS MUST NOT be
retried past the breaker.
**Verified by** `TestEdge003_OperationalUnavailable_Fallback`, `TestEdge003_OperationalUnavailable_NoFallback` in `test/integration/edge_health_test.go`.

### REQ-EDGE-004 — Execution source unavailable
**MUST.** For `EXECUTION`-targeted request types with `fallback: none`
(`execution_status`, `execution_history`) ⇒ `503 UPSTREAM_UNAVAILABLE`; no
operational substitution is permitted because ODS cannot answer those field
groups. For `BOTH` where execution is optional ⇒ `200` with `partial = true`,
warning `SOURCE_UNAVAILABLE`, `partial_response_total` incremented.
**Verified by** `TestEdge004_ExecutionUnavailable_Required`, `TestEdge004_ExecutionUnavailable_Optional` in `test/integration/edge_health_test.go`.

### REQ-EDGE-005 — Both sources unavailable
**MUST.** Rule `health.both_unavailable`. If stale-serve is allowed and a cache
entry exists within `max_stale` ⇒ `200`, `degraded = true`, warnings
`STALE_DATA` + per-source unavailability. Otherwise `503
NO_SOURCE_AVAILABLE` listing both source states, `retryable: true`.
**Verified by** `TestEdge005_BothUnavailable_StaleServe`, `TestEdge005_BothUnavailable_HardFail` in `test/integration/edge_health_test.go`.

### REQ-EDGE-006 — Operational response partially populated
**MUST.** Missing optional groups (no metrics, empty topology) ⇒ the canonical
model omits them; they MUST NOT be emitted as zero values. If a *required* field
group for the request type is missing, the response is `206` with `partial=true`
and warning `PARTIAL_DATA`. Empty groups MUST NOT displace populated
values from the other source (REQ-PREC-005).
**Verified by** `TestEdge006_OperationalPartial` in `test/integration/edge_partial_test.go`.

### REQ-EDGE-007 — Execution response partially populated
**MUST.** An execution record missing `steps`, `result` or `error` maps with
those fields absent; `status` still maps. A `RUNNING` execution with no `result`
is normal and MUST NOT warn. A `COMPLETED` execution with neither `result` nor
`error` warns `PARTIAL_DATA`.
**Verified by** `TestEdge007_ExecutionPartial` in `test/integration/edge_partial_test.go`.

### REQ-EDGE-008 — Conflicting values between sources
**MUST.** Both sources supply `status` with different values ⇒
`SourcePrecedencePolicy` picks per REQ-PREC-002/003, `precedence_conflict_total`
increments, warning `CONFLICT_RESOLVED` names the losing source, and
`meta.provenance.status` names the winner. The response MUST NOT contain both
values or an "either" union.
**Verified by** `TestEdge008_ConflictResolution` in `test/integration/edge_conflict_test.go`.

### REQ-EDGE-009 — Different timestamps between sources
**MUST.** Divergent `observedAt` values do NOT change precedence (REQ-PREC-006).
`meta.freshness` reports the *oldest* contributing observation for a multi-source
response, so freshness is not overstated. Per-source observation times are
available in the per-source provenance detail.
**Verified by** `TestEdge009_OldestObservationWins` in `test/integration/edge_timestamp_test.go`.

### REQ-EDGE-010 — Clock skew between BFF and source
**MUST.** Implement REQ-TTL-004: prefer same-clock-domain age
(`server_time - last_updated`); clamp fallback skew to `clock_skew_tolerance`;
clamp negative age to zero with `CLOCK_SKEW_DETECTED`; force `UNKNOWN` on gross
skew. A source running 10 s fast MUST NOT make 40 s-old data look fresh under a
30 s TTL.
**Verified by** `TestEdge010_SkewDoesNotFakeFreshness`, `TestEdge010_NegativeAgeClamped` in `test/integration/edge_skew_test.go`.

### REQ-EDGE-011 — Cache holds stale data
**MUST.** A cache hit is re-evaluated against `observedAt` (REQ-CACHE-009). If
stale and stale-serve is disallowed, the entry is bypassed (and MAY be evicted)
and the source is consulted; if allowed, it is served with `degraded = true`,
`cache.hit = true`, warning `STALE_DATA`, and
`stale_response_total` incremented.
**Verified by** `TestEdge011_CacheStaleReevaluated` in `test/integration/edge_cache_test.go`.

### REQ-EDGE-012 — Concurrent requests for the same resource
**MUST.** N concurrent identical requests produce at most one upstream read per
source (singleflight, REQ-CACHE-004). All N receive equivalent responses; each
gets its own `correlationId`. The shared upstream call's cancellation MUST NOT be
triggered by one waiter disconnecting.
**Verified by** `TestEdge012_ConcurrentSingleflight`, `TestEdge012_WaiterCancelDoesNotAbortLeader` in `test/integration/edge_concurrency_test.go`.

### REQ-EDGE-013 — Source timeout
**MUST.** A source exceeding `PerSourceTimeout` yields `UPSTREAM_TIMEOUT`
(retryable, degradable). If it is optional ⇒ `200 partial` with
`SOURCE_TIMEOUT`. If required ⇒ the degradation ladder (REQ-RES-009);
final failure is `504`-class mapped to `UPSTREAM_TIMEOUT` with status `504`.
Timeouts count toward the breaker.
**Verified by** `TestEdge013_SourceTimeoutOptional`, `TestEdge013_SourceTimeoutRequired` in `test/integration/edge_timeout_test.go`.

### REQ-EDGE-014 — Network failure
**MUST.** Connection refused, DNS failure, TLS handshake failure and mid-stream
reset all map to `UPSTREAM_UNAVAILABLE` (retryable). Retry applies with full
jitter within budget; exhaustion feeds the breaker and the degradation ladder. A
mid-stream reset MUST NOT yield a partially decoded record.
**Verified by** `TestEdge014_NetworkFailureClasses`, `TestEdge014_NoPartialDecodeOnReset` in `test/integration/edge_network_test.go`.

### REQ-EDGE-015 — Execution currently running
**MUST.** When the latest execution is `RUNNING`: (a) precedence override applies
to `status`/`subState` (REQ-PREC-003); (b) the response is *not* marked
degraded merely because the operational record lags; (c) `cache_ttl` for the
affected keys is clamped to `min(cache_ttl, 2s)` so a fast-changing resource is
not pinned; (d) warning `CONFLICT_RESOLVED` only if values actually differ.
**Verified by** `TestEdge015_RunningExecutionOverride`, `TestEdge015_CacheTTLClamped` in `test/integration/edge_running_test.go`.

### REQ-EDGE-016 — Tenant isolation
**MUST.** Tenant A's token MUST NOT retrieve tenant B's resource: cache keys are
disjoint (REQ-CACHE-002), source calls carry the authenticated tenant, and a
source record whose `tenantId` differs is rejected with `TENANT_MISMATCH`
(REQ-SEC-006) and an audit event. Applies to cached and uncached paths.
**Verified by** `TestEdge016_TenantIsolationCached`, `TestEdge016_TenantIsolationUncached`, `TestEdge016_SourceReturnsForeignTenant` in `test/integration/edge_tenant_test.go`.

### REQ-EDGE-017 — Schema version mismatch
**MUST.** Sources declare `schema_version` (ODS field 15; EDS
`schemaVersion` / `X-Schema-Version`). A **minor** version ahead of the BFF's
supported version is tolerated: unknown fields are ignored, unknown enum members
map to `UNKNOWN`, and warning `SCHEMA_VERSION_MISMATCH` is emitted. A **major**
mismatch is fatal for that call: `SCHEMA_VERSION_MISMATCH` (terminal, not
retryable).

**It MUST NOT trip the source's circuit breaker.** `resilience.isClientFault`
lists `SCHEMA_VERSION_MISMATCH` alongside the caller-fault codes, so the breaker
records it as a success and the source stays `HEALTHY` for routing purposes. The
source is up, fast and answering correctly; it is the BFF that does not
understand the contract. Tripping the breaker would open a circuit on a healthy
source, silently reroute every request to the slower one, and replace a specific
version-incompatibility alert with a vague availability one.

It surfaces instead through `schema_version_mismatch_total`, through a `502`
where the request type has no fallback, and through the **call-time** fallback
where it has one: `errs.SourceUnusable` returns true for this code, so
`fallback.primary_failed` fires and the request succeeds `200` degraded.
**Verified by** `TestEdge017_MinorDriftTolerated`, `TestEdge017_MajorMismatchFatal`, `TestEdge017_MismatchAllowsFallback` in `test/integration/edge_schema_test.go`.

### REQ-EDGE-018 — Partial response
**MUST.** When some but not all requested field groups are obtainable, the BFF
returns the obtainable ones with `partial = true`, per-field `provenance`, and
one warning per missing source naming its cause. `partial` MUST also be set when
the source that *did* answer cannot hold every requested field — a field counts
as unsatisfiable only when **none** of its catalogued suppliers is in the chosen
target, so a fallback to the execution source makes `/resources/{id}` partial but
leaves `/status` complete. **Any** partial answer is `206`:
`response.Writer.OK` derives the status from `meta.partial` alone, so there is no
`200 partial: true` case (REQ-AGG-004). A degraded-but-complete answer stays
`200` with `meta.degraded: true`, because the data is whole and only older than
intended. Missing groups are absent, never null-filled.
**Verified by** `TestEdge018_PartialResponseShape` in `test/integration/edge_partial_test.go`.

### REQ-EDGE-019 — Empty result
**MUST.** Distinguish three cases: (a) resource not found in every consulted
source ⇒ `404 NOT_FOUND`, negatively cached; (b) resource found with an empty
collection (no executions) ⇒ `200` with `data.executions: []`, warning
no warning, and **not** `404`; (c) source returned success with an empty body
⇒ `UPSTREAM_INVALID_RESPONSE`. An empty collection MUST NOT be negatively cached
as not-found.
**Verified by** `TestEdge019_NotFound`, `TestEdge019_EmptyCollectionIs200`, `TestEdge019_EmptyBodyIsInvalid` in `test/integration/edge_empty_test.go`.

### REQ-EDGE-020 — Invalid source response
**MUST.** Malformed JSON, wrong content type, proto decode failure, missing
mandatory identity fields, `NaN`/`Inf` numerics, or a timestamp far outside a
sane window ⇒ `UPSTREAM_INVALID_RESPONSE` (terminal, not retried, counted in
`datasource_error_total{outcome="invalid"}`). The offending payload is logged
truncated and redacted, never returned to the client. If the failing source is
optional, the request continues as partial.
**Verified by** `TestEdge020_InvalidPayloadClasses`, `TestEdge020_NotRetried`, `TestEdge020_OptionalSourceContinues` in `test/integration/edge_invalid_test.go`.

---

## 17. Traceability matrix

### 17.1 Requirement family → package

| Family | `internal/` package(s) | `pkg/` | Primary test location |
|---|---|---|---|
| REQ-API | `api`, `api/handler`, `api/middleware`, `api/response` | `correlation`, `errs` | `internal/api/**/*_test.go` |
| REQ-CLS | `classifier` | — | `internal/classifier/*_test.go` |
| REQ-RT | `router`, `policy` | — | `internal/router/*_test.go` |
| REQ-TTL | `freshness`, `config` | — | `internal/freshness/*_test.go` |
| REQ-PREC | `policy` | — | `internal/policy/*_test.go` |
| REQ-AGG | `aggregation`, `application` | — | `internal/aggregation/*_test.go` |
| REQ-MAP | `mapper`, `domain` | — | `internal/mapper/*_test.go` |
| REQ-DS | `datasource`, `datasource/operational`, `datasource/execution` | `errs` | `internal/datasource/**/*_test.go`, `test/contract` |
| REQ-CACHE | `cache` | — | `internal/cache/*_test.go` |
| REQ-RES | `resilience`, `application` | `errs` | `internal/resilience/*_test.go` |
| REQ-OBS | `observability` | `correlation` | `internal/observability/*_test.go` |
| REQ-SEC | `security`, `api/middleware`, `config` | — | `internal/security/*_test.go` |
| REQ-MT | `config`, `cache`, `security`, `observability` | `correlation` | `internal/config/tenant_test.go`, `internal/cache/key_test.go` |
| REQ-CFG | `config` | — | `internal/config/*_test.go` |
| REQ-PERF | cross-cutting | — | `test/load`, `internal/**/bench_test.go` |
| REQ-EDGE | cross-cutting | — | `test/integration/edge_*_test.go` |

### 17.2 Package → owning requirements

| Package | Owns | Must not depend on |
|---|---|---|
| `internal/domain` | REQ-MAP-001, canonical vocabulary | anything in this module or any transport library |
| `internal/classifier` | REQ-CLS-001..005 | `datasource`, `cache`, `api` |
| `internal/freshness` | REQ-TTL-001..010, REQ-EDGE-010 | `api`, `cache` |
| `internal/router` | REQ-RT-001..010, REQ-EDGE-001..005 | `api`, `mapper` |
| `internal/policy` | REQ-PREC-001..007, field catalog | `datasource`, `api` |
| `internal/aggregation` | REQ-AGG-001..007, REQ-EDGE-018 | `api` |
| `internal/mapper` | REQ-MAP-002..010, REQ-EDGE-006/007/017/020 | `api`, `cache`, `router` |
| `internal/datasource/**` | REQ-DS-001..010, REQ-EDGE-013/014/020 | `api`, `application` |
| `internal/cache` | REQ-CACHE-001..010, REQ-EDGE-011/012 | `api`, `datasource` |
| `internal/resilience` | REQ-RES-001..012 | `api`, `domain`-mutating code |
| `internal/security` | REQ-SEC-001..014, REQ-MT-001 | `datasource` |
| `internal/observability` | REQ-OBS-001..010 | `application` |
| `internal/config` | REQ-CFG-001..007, REQ-MT-002 | everything else |
| `internal/application` | request lifecycle, REQ-RES-009, REQ-API-007 | `datasource` adapters (ports only) |
| `internal/api/**` | REQ-API-001..015 | `datasource` adapters |
| `pkg/errs` | REQ-RES-004, REQ-API-009 | anything under `internal/` |
| `pkg/correlation` | REQ-API-010, REQ-MT-001 | anything under `internal/` |

### 17.3 Edge case → rule / mechanism

| Edge | Mechanism | Rule id involved |
|---|---|---|
| 001 | freshness verdict FRESH | `ttl.operational.fresh` |
| 002 | freshness verdict STALE | `ttl.operational.stale` |
| 003 | health snapshot | `health.primary_unavailable` |
| 004 | required/optional source map | `fields.execution_only`, aggregation |
| 005 | health snapshot | `health.both_unavailable` |
| 006 | mapper optional groups | — |
| 007 | mapper optional groups | — |
| 008 | precedence policy | — |
| 009 | oldest-observation freshness | — |
| 010 | skew correction | `ttl.unknown_freshness` (gross skew) |
| 011 | cache entry re-evaluation | any TTL rule |
| 012 | singleflight | — |
| 013 | per-source timeout | `health.primary_unavailable` on repeat |
| 014 | adapter error translation + retry | `health.primary_unavailable` |
| 015 | execution-overrides-when-running | — |
| 016 | tenant key + assertion | `guard.tenant_missing` |
| 017 | schema version gate | `health.primary_unavailable` (major) |
| 018 | aggregation partial | `fields.span_both` |
| 019 | not-found vs empty collection | — |
| 020 | mapper validation | `health.primary_unavailable` (on repeat) |
