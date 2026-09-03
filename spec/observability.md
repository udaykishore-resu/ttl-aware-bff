# TTL-Aware BFF — Observability

Every metric, span, log field, SLI and alert. Instrument names, attribute names
and span names are frozen by `docs/DESIGN-CONTRACT.md` §7–§8. Requirements from
`spec/requirements.md`.

Implementation lives in `internal/observability`: OTel tracer and meter
providers, the instrument registry, the `slog` JSON handler, and the sampler.
Export is OTLP to the ADOT collector plus a Prometheus scrape endpoint on the
admin listener at `/metrics` (REQ-API-002).

---

## 1. Design position

**Instrumentation exists to answer a bounded set of questions.** Every instrument
below is listed with the question it answers; an instrument that answers no
stated question is deleted rather than kept "just in case". The questions the
service must be able to answer without reading code are:

1. Are we serving? At what latency, per route class?
2. Is the TTL policy actually avoiding the expensive source, and by how much?
3. When we fall back, degrade or return partial data — why, and how often?
4. Which routing rule fired, and did its distribution change?
5. Which source is failing, in what way, and is protection engaged?
6. How old is the data users are actually seeing?
7. Which tenant is responsible for a change in any of the above?

**Cardinality is a design constraint, not a tuning detail.** The attribute
allow-list in §3 is enforced by a test (REQ-OBS-002), because unbounded label
cardinality is the dominant cost and the dominant outage risk in metrics
backends.

---

## 2. Metrics

Unit convention: histograms are **seconds** (`s`) as float64; counters are
dimensionless (`{request}`, `{error}`, …).

### 2.1 Counters

| Metric | Unit | Attributes | Question it answers | Req |
|---|---|---|---|---|
| `bff_request_total` | `{request}` | `request_type`, `routing_decision`, `http_status`, `outcome`, `degraded`, `partial`, `tenant_id` | What is the request rate and error rate per endpoint and tenant? | REQ-OBS-001 |
| `operational_ttl_hit_total` | `{verdict}` | `request_type`, `tenant_id` | How often is the operational record fresh enough to use? **This is the system's headline effectiveness metric.** | REQ-OBS-008 |
| `operational_ttl_miss_total` | `{verdict}` | `request_type`, `tenant_id`, `outcome` ∈ `stale\|unknown` | How often does freshness fail, and is it staleness or unknowability? | REQ-OBS-008 |
| `execution_fallback_total` | `{fallback}` | `request_type`, `routing_rule`, `tenant_id` | How often are we paying the expensive source because the cheap one was inadequate? | REQ-EDGE-002 |
| `datasource_error_total` | `{error}` | `source`, `outcome` ∈ `timeout\|unavailable\|invalid\|not_found\|schema_mismatch`, `tenant_id` | Which source is failing and in what way? | REQ-DS-007 |
| `cache_hit_total` | `{lookup}` | `layer` ∈ `L1\|L2`, `request_type`, `tenant_id` | Is the cache earning its memory and its Redis bill? | REQ-CACHE-010 |
| `cache_miss_total` | `{lookup}` | `layer`, `request_type`, `tenant_id` | |
| `partial_response_total` | `{response}` | `request_type`, `source`, `tenant_id` | How often is an optional source missing from responses? | REQ-AGG-004 |
| `stale_response_total` | `{response}` | `request_type`, `source`, `tenant_id` | How often are users seeing knowingly-old data? | REQ-TTL-008 |
| `routing_decision_total` | `{decision}` | `routing_decision`, `routing_rule`, `request_type`, `tenant_id` | Which rule fired? **The primary diagnostic for behaviour change.** | REQ-RT-009 |
| `circuit_breaker_transition_total` | `{transition}` | `source`, `state` ∈ `open\|half_open\|closed` | When did protection engage or release? | REQ-RES-005 |
| `precedence_conflict_total` | `{conflict}` | `field_group`, `winner`, `loser`, `tenant_id` | How often do the two sources actually disagree, and about what? | REQ-PREC-004 |

**`operational_ttl_hit_total` semantics (REQ-OBS-008).** It counts *freshness
verdicts*, not cache outcomes. It increments only when the verdict was `FRESH`
**and** the operational source was consequently used. A cache hit that avoided
the source entirely does not increment it — that is `cache_hit_total`.
Conflating the two would make the TTL policy look effective when what is actually
working is the cache, and would hide a TTL that is set uselessly high.

### 2.2 Histograms (seconds)

| Metric | Attributes | Suggested buckets (s) | Question it answers | Req |
|---|---|---|---|---|
| `bff_request_latency` | `request_type`, `routing_decision`, `http_status`, `degraded`, `partial`, `tenant_id` | .001 .0025 .005 .01 .025 .05 .1 .25 .5 1 2.5 5 | End-to-end latency per route class — the SLI (REQ-PERF-001) | REQ-OBS-001 |
| `operational_source_latency` | `source=OPERATIONAL`, `outcome`, `rpc` ∈ `GetResource\|GetResourceState\|GetResourceFreshness\|BatchGetResources` | .001 .0025 .005 .01 .025 .05 .1 .25 .5 1 | Is the ODS meeting its budget? Is the **probe actually cheaper than the read**? | REQ-PERF-002 |
| `execution_source_latency` | `source=EXECUTION`, `outcome`, `endpoint` | .005 .01 .025 .05 .1 .25 .5 1 2.5 5 | Is the EDS meeting its budget? | REQ-PERF-001 |
| `aggregation_latency` | `request_type`, `partial` | .001 .005 .01 .025 .05 .1 .25 .5 1 2.5 | Is fan-out costing `max(branches)` rather than their sum? | REQ-AGG-006, REQ-PERF-006 |
| `data_freshness_age` | `request_type`, `source`, `tenant_id` | .1 .5 1 2.5 5 10 30 60 120 300 600 | **How old is the data users actually saw?** The staleness SLI. | REQ-TTL-009 |

`data_freshness_age` is recorded on every served response, including cache hits
and degraded responses, using the skew-corrected age of the observation that
backed the answer. It is the only instrument that measures the property the whole
service exists to manage, and it is what makes a TTL change's effect visible.

Comparing `operational_source_latency{rpc="GetResourceFreshness"}` against
`{rpc="GetResource"}` is the live verification of REQ-PERF-002 — the economic
premise of the design. If the probe stops being materially cheaper, the routing
strategy needs revisiting, and this comparison is how that is noticed.

### 2.3 Gauges / UpDownCounters

| Metric | Attributes | Question it answers | Req |
|---|---|---|---|
| `bff_concurrent_requests` | `request_type` | Are we saturating? The HPA signal alongside CPU. | REQ-PERF-005 |
| `bulkhead_in_flight` | `source` | Is one source consuming its concurrency budget? | REQ-RES-007 |
| `circuit_breaker_state` | `source` | Current protection state: `0` closed, `1` half-open, `2` open. | REQ-RES-005 |

Supplementary gauges (not in the frozen contract set, exported for capacity
planning): L1 entry count, JWKS key-set age, config snapshot generation.

---

## 3. Attribute allow-list and cardinality control

### 3.1 Permitted attributes

| Attribute | Values | Bound |
|---|---|---|
| `tenant_id` | configured tenants | collapses to `_other` beyond `observability.max_tenant_cardinality` (REQ-MT-004) |
| `request_type` | 6 values | fixed by the endpoint set (REQ-API-001) |
| `routing_decision` | 4 values | `OPERATIONAL\|EXECUTION\|BOTH\|NONE` |
| `routing_rule` | 11 values | the frozen rule ids |
| `source` | 4 values | `OPERATIONAL\|EXECUTION\|CACHE\|NONE` |
| `outcome` | ~8 values | per-instrument enumeration |
| `http_status` | ~12 values | the statuses this API emits |
| `degraded`, `partial` | 2 values each | booleans |
| `layer` | 3 values | `L1\|L2\|NONE` (cache only) |
| `field_group` | 9 values | the field catalog (precedence only) |
| `winner`, `loser` | 2 values each | source kinds (precedence only) |
| `state` | 3 values | breaker states |
| `rpc`, `endpoint` | ≤ 5 values each | per-source method names |

### 3.2 Prohibited as metric attributes (REQ-OBS-002)

`resourceId`, `executionId`, `correlationId`, `trace_id`, `principal`,
`customerId`, URL path, user agent, error `detail`, upstream hostname, and any
free text from a source payload.

These belong on **spans** (where cardinality is per-trace, not per-series) and in
**logs** (where it is per-line). Putting `resourceId` on a metric creates one
time series per resource, which is unbounded by construction.

Worst-case series count for the frozen set, with 50 tenants:

```
bff_request_total: 6 request_type × 4 decision × 12 status × 8 outcome
                   × 2 degraded × 2 partial × 50 tenants ≈ 460k  ← too many
```

which is why `outcome` and `http_status` are constrained to co-vary (only the
combinations the status mapping can actually produce are emitted), reducing the
realistic cardinality to a few thousand series. The test asserts the attribute
*keys*; the emission sites are responsible for not producing impossible
combinations.

### 3.3 Enforcement

`TestMetrics_NoHighCardinalityAttributes` (`internal/observability/cardinality_test.go`)
scans every instrument's recorded attribute keys against the allow-list and
fails on any key outside it. `TestMetrics_TenantCardinalityCollapse` asserts the
`_other` collapse. `TestMetrics_InstrumentInventory` asserts the instrument set
matches the contract exactly — no additions, no omissions.

---

## 4. Span tree

Frozen by the contract (REQ-OBS-003). The shipped set is deliberately small:

```
<route pattern>                       e.g. "GET /api/v1/resources/{resourceId}/status"
├── bff.usecase.resource | bff.usecase.execution
│   ├── bff.route
│   ├── bff.aggregate
│   └── bff.resolve_in_flight         only when the ODS record declares an
                                      in_flight_execution_ref
```

The server span is produced by `otelhttp` with a span-name formatter that returns
the matched **route pattern**, never the raw path — an unmatched request is named
`<METHOD> unmatched`. That is what keeps trace cardinality bounded the same way
metric attributes are (REQ-OBS-013): the path contains resource ids, the pattern
does not.

`bff.aggregate` is where the fan-out happens, so for `Target = BOTH` the
concurrency shows up as its own duration being the *maximum* of its branches
rather than their sum — which is how REQ-PERF-006 is verified by eye.

There is deliberately **no** span per source call, per mapper invocation, per
cache lookup or per precedence decision. Those are recorded as metrics
(`operational_source_latency`, `execution_source_latency`, `cache_hit_total`,
`precedence_conflict_total`) instead. A read path this hot should not pay for
span construction on every internal step, and the interesting questions — which
rule fired, what the freshness verdict was, how long the fan-out took — are all
answerable from the five spans above plus the response envelope.

### 4.1 Span attributes

| Span | Attributes |
|---|---|
| `<route pattern>` | the standard `otelhttp` HTTP server attributes |
| `bff.usecase.resource` | `request_type`, `view` ∈ `full`/`status`/`configuration`/`details` |
| `bff.usecase.execution` | `request_type` |
| `bff.route` | `routing_target`, `routing_rule`, `freshness` |
| `bff.aggregate` | `routing_target`, `routing_rule` |
| `bff.resolve_in_flight` | `execution_id` |

The cache key never appears on a span: it contains `tenantId` and `resourceId`,
and traces are frequently shared more widely than logs.

### 4.2 Span status and errors (REQ-OBS-004)

| Situation | Span status | Extra |
|---|---|---|
| success | `Ok` | |
| success but degraded | `Ok` | `bff.degraded = true` |
| success but partial | `Ok` | `bff.partial = true` |
| handled error (4xx) | `Ok` on `bff.request`, `Error` on the originating child span | `error.code` |
| server error (5xx) | `Error` | `RecordError(err)`, `error.code`, `error.type` |
| source failure that was degraded around | `Error` on the source span, `Ok` on `bff.request` | the child records the truth, the parent records the outcome |

The distinction in the last row matters: a request that degraded successfully is
not an error trace, but its source span must still show the failure, or the
degradation becomes invisible.

### 4.3 Sampling (REQ-OBS-010)

`ParentBased(TraceIDRatioBased(observability.trace_sample_ratio))`. Parent
decisions are always respected so a trace is never truncated mid-flight.

Raised-rate sampling for the traces that matter most: requests that end
`degraded`, `partial`, or with a `5xx` are sampled at a higher ratio via a
composite sampler that consults the recorded outcome. Because the outcome is
known only at the end, this is implemented as tail-ish behaviour in the
collector (an OTel tail-sampling policy on `bff.degraded`, `bff.partial` and
span status) rather than in-process — the in-process sampler stays head-based
and cheap.

---

## 5. Log schema

`slog` with a JSON handler (REQ-OBS-005). Exactly **one** request-completion line
per request; everything else is debug or a discrete event (audit, config reload,
breaker transition).

### 5.1 Request-completion line

| Field | Type | Notes |
|---|---|---|
| `ts` | RFC 3339 UTC, ms | |
| `level` | string | `info` normally, `warn` when degraded/partial, `error` on 5xx |
| `msg` | string | `"request completed"` |
| `service` | string | `observability.service_name` |
| `version` | string | `observability.service_version` |
| `env` | string | `observability.environment` |
| `correlation_id` | string | joins to `meta.correlationId` and to both sources |
| `trace_id`, `span_id` | hex | joins logs to traces |
| `tenant_id` | string | |
| `principal` | string | JWT subject; not the token |
| `http_method`, `http_route`, `http_status` | | `http_route` is the **template**, never the filled path |
| `request_type` | string | |
| `routing_decision`, `routing_rule`, `routing_reason` | string | the routing `Decision` verbatim (REQ-RT-005) |
| `freshness_state`, `freshness_age_seconds`, `freshness_ttl_seconds` | | |
| `sources` | array | sources that contributed |
| `source_latency_ms` | object | per-source, e.g. `{"OPERATIONAL": 41, "EXECUTION": 238}` |
| `cache_hit`, `cache_layer` | | |
| `degraded`, `partial` | bool | |
| `warnings` | array of codes | codes only, not messages |
| `error_code` | string | present on failure |
| `duration_ms` | number | |
| `resource_id`, `execution_id` | string | permitted **in logs** (bounded per line), never in metrics |

### 5.2 Discrete event lines

| Event | Level | Extra fields |
|---|---|---|
| `auth failure` | warn | `reason`, `issuer`, `kid` — never the token |
| `authz denial` | warn | `required_permission`, `roles` |
| `tenant mismatch` | error | `claim_tenant`, `header_tenant` |
| `cross-tenant record rejected` | error | `source`, `record_tenant` — a security event |
| `rate limited` | warn | `limit`, `burst` |
| `breaker transition` | warn | `source`, `from`, `to`, `failure_ratio` |
| `config reload` | info | `generation`, `changed_keys` (names only, no values) |
| `config reload rejected` | error | `validation_error` |
| `upstream invalid payload` | error | `source`, `payload_excerpt` (≤ 512 B, redacted) |
| `schema version drift` | warn | `source`, `reported`, `supported` |
| `goroutine panic recovered` | error | `stack` |

### 5.3 Redaction (REQ-OBS-006, REQ-SEC-013)

Never logged, at any level: `Authorization` header values, JWT contents, JWKS
private material, Redis password, HS256 secret, any config value whose key
matches `password|secret|token|key`, and configuration map values whose keys
match the tenant's configured secret patterns.

`payload_excerpt` passes through the same redaction pass before truncation.
`TestLogging_RedactsSecrets` emits lines with planted secrets and asserts none
appear.

### 5.4 Level policy

| Level | Use |
|---|---|
| `error` | 5xx, security events, invalid upstream payloads, rejected config |
| `warn` | degraded/partial responses, breaker transitions, rate limiting, auth failures, schema drift |
| `info` | request completion, lifecycle (start, ready, reload, shutdown) |
| `debug` | routing predicate evaluation, effective TTL resolution, probe memo hits, per-field precedence decisions |

Debug is off in production by default and is enabled per-tenant via config reload
during an investigation, without a restart (REQ-CFG-004).

---

## 6. Exemplars

`bff_request_latency`, `operational_source_latency`, `execution_source_latency`
and `aggregation_latency` attach trace exemplars when the span is sampled
(REQ-OBS-007). The exemplar carries `trace_id`, `span_id` and the `tenant_id`
attribute set.

This is what turns "P99 is 800 ms" into "here is a trace of an 800 ms request".
Without exemplars, the histogram tells you a tail exists and nothing about what
is in it. Exemplars are attached only for sampled spans, so exemplar coverage of
the tail depends on the raised-rate sampling in §4.3 — a deliberate pairing.

**Verified by** `TestMetrics_ExemplarsAttached`.

---

## 7. SLIs and SLOs

Measured over a rolling 28-day window, per environment. Multi-window multi-burn-rate
alerting (2%/1h and 5%/6h fast burn; 10%/3d slow burn).

### 7.1 Availability

| SLI | Definition | SLO |
|---|---|---|
| API availability | `1 − (bff_request_total{http_status=~"5.."} / bff_request_total)` excluding `429` | **99.9%** |
| Answerability | `1 − (bff_request_total{outcome="no_source_available"} / bff_request_total)` | **99.95%** |

Answerability is tracked separately from availability because
`NO_SOURCE_AVAILABLE` means the degradation ladder ran to exhaustion — a
categorically worse failure than a single upstream error that was degraded
around, and one whose budget should be far tighter.

### 7.2 Latency, per route class (REQ-PERF-001)

| Route class | P95 SLO | P99 SLO | Series filter |
|---|---|---|---|
| cache hit | 5 ms | 15 ms | `bff_request_latency` where `cache_hit=true` |
| `OPERATIONAL` | 60 ms | 120 ms | `routing_decision="OPERATIONAL"` |
| `EXECUTION` | 250 ms | 500 ms | `routing_decision="EXECUTION"` |
| `BOTH` | 280 ms | 550 ms | `routing_decision="BOTH"` |

Latency SLOs are stated per route class rather than globally because a shift in
the routing mix would otherwise silently blow or silently rescue a global
target — the number would move for reasons unrelated to service health.

Supporting budgets: probe P95 ≤ 15 ms (REQ-PERF-002); BFF-attributable overhead
P99 ≤ 12 ms for `BOTH`, ≤ 6 ms otherwise (REQ-PERF-003).

### 7.3 Staleness ratio

| SLI | Definition | SLO |
|---|---|---|
| Staleness ratio | `stale_response_total / bff_request_total` | **< 1%** |
| Freshness age P95 | `histogram_quantile(0.95, data_freshness_age)` per `request_type` | **< 2 × effective TTL** |

The second is the more informative of the two. A staleness ratio near zero with
a P95 age near the TTL means the TTL is tuned close to the source's actual
refresh cadence — the desired state. A P95 age far *below* the TTL means the TTL
is set uselessly high and could be tightened for better data at no cost.

### 7.4 Fallback ratio

| SLI | Definition | SLO |
|---|---|---|
| Execution fallback ratio | `execution_fallback_total / bff_request_total{routing_decision!="EXECUTION"}` | **< 5%** |
| TTL hit ratio | `operational_ttl_hit_total / (operational_ttl_hit_total + operational_ttl_miss_total)` | **> 90%** |
| Partial response ratio | `partial_response_total / bff_request_total` | **< 2%** |
| Cache hit ratio | `cache_hit_total / (cache_hit_total + cache_miss_total)` | **> 60%** (informational) |

The fallback ratio is a **cost and latency** SLI, not an availability one. Every
point of fallback shifts requests from the 60 ms ODS profile to the 250 ms EDS
profile and consumes scarce EDS bulkhead capacity. A rising fallback ratio with
flat availability is the signature of an ODS whose refresh cadence has slipped
past the configured TTL — the exact condition the service is designed to make
visible.

TTL hit ratio and fallback ratio are near-complements but not identical:
`ttl.unknown_freshness` produces a miss without producing a fallback. The gap
between them is the probe's failure rate.

### 7.5 Correctness signals (no SLO, alert on any occurrence)

| Signal | Meaning |
|---|---|
| `precedence_conflict_total` rate | sources disagreeing; a rise means a real divergence, not a BFF bug |
| `datasource_error_total{outcome="invalid"}` | a source shipped a breaking change |
| `SCHEMA_VERSION_MISMATCH` | major contract break |
| `TENANT_MISMATCH` from a source record | a source returned foreign-tenant data — security incident |
| `CLOCK_SKEW_DETECTED` warning rate | a source's clock is drifting; freshness verdicts are degrading toward `UNKNOWN` |

---

## 8. Alerts

| # | Alert | Condition | Severity | Rationale |
|---|---|---|---|---|
| 1 | `BFFAvailabilityFastBurn` | error-budget burn > 14.4× over 1h and 5m | page | 2% of a 30-day budget in 1h |
| 2 | `BFFAvailabilitySlowBurn` | burn > 6× over 6h and 30m | ticket | |
| 3 | `BFFNoSourceAvailable` | `rate(bff_request_total{outcome="no_source_available"}) > 0.001 × total` for 5m | page | the ladder is exhausting |
| 4 | `BFFLatencyP99` | P99 > 2× the route-class SLO for 10m | page | scoped per `routing_decision` |
| 5 | `BFFFallbackRatioHigh` | execution fallback ratio > 15% for 15m | ticket | cost/latency regression; usually ODS refresh slippage |
| 6 | `BFFTTLHitRatioLow` | TTL hit ratio < 70% for 30m | ticket | the TTL policy has stopped working |
| 7 | `BFFStalenessHigh` | staleness ratio > 5% for 15m | ticket | users are seeing knowingly-old data |
| 8 | `BFFFreshnessAgeExceedsTTL` | P95 `data_freshness_age` > 2× TTL for 15m, per `request_type` | ticket | source refresh cadence has slipped |
| 9 | `BFFCircuitOpen` | `circuit_breaker_state == 2` for > 2 cooldown periods | page | the upstream is not recovering |
| 10 | `BFFPartialResponses` | partial ratio > 5% for 15m | ticket | an optional source is effectively down |
| 11 | `BFFUpstreamInvalid` | `rate(datasource_error_total{outcome="invalid"}) > 0` for 5m | page | not self-healing; a source shipped a breaking change |
| 12 | `BFFSchemaMismatch` | any `SCHEMA_VERSION_MISMATCH` | page | major contract break. Note the breaker will **not** move: client faults and schema mismatches abstain from it, so this metric is the only signal |
| 12a | `BFFUnconfiguredRequestType` | any `routing_decision_total{routing_rule="guard.unconfigured_request_type"}` | page | a request type has no routing rule at all — a broken deployment, not a routing outcome |
| 13 | `BFFTenantMismatch` | any cross-tenant record rejection | page | security incident |
| 14 | `BFFClockSkew` | `CLOCK_SKEW_DETECTED` warning rate > 1% for 30m | ticket | freshness verdicts degrading |
| 15 | `BFFBulkheadSaturated` | `bulkhead_in_flight / max_concurrent > 0.9` for 10m | ticket | capacity or an upstream slowdown |
| 16 | `BFFCacheL2Down` | `cache_miss_total{layer="L2"}` ≈ total L2 lookups for 10m | ticket | running L1-only; origin load will rise with replica count |
| 17 | `BFFRateLimitHigh` | `429` ratio > 1% for a single tenant for 15m | ticket | a tenant needs a limit review or is misbehaving |
| 18 | `BFFPrecedenceConflicts` | conflict rate > 10× the 7-day baseline | ticket | genuine source divergence |
| 19 | `BFFReadinessFlapping` | `/readyz` flips > 5× in 10m | ticket | dependency instability causing pod churn |
| 20 | `BFFConfigReloadRejected` | any rejected reload | ticket | operators believe a change applied when it did not |

Alerts 3, 11, 12 and 13 page on conditions that do **not** self-heal. Alerts 5,
6, 7 and 8 are the TTL-policy health family — they are the ones that would be
missing from a conventional BFF's alert set, and they are the ones that detect
the failure mode this service is specifically built to manage.

---

## 9. Dashboards

| Panel | Query basis | Purpose |
|---|---|---|
| Request rate and error rate by `request_type` | `bff_request_total` | overview |
| Latency P50/P95/P99 by `routing_decision` | `bff_request_latency` | SLO tracking, mix-independent |
| **Routing rule distribution over time** | `routing_decision_total` by `routing_rule` | the first panel to open when behaviour changes |
| TTL hit vs miss, and miss split stale/unknown | `operational_ttl_*_total` | policy effectiveness |
| Freshness age heatmap by `request_type` | `data_freshness_age` | what users actually see |
| Fallback ratio and EDS call rate | `execution_fallback_total`, `execution_source_latency` count | cost |
| Probe vs read latency | `operational_source_latency` by `rpc` | REQ-PERF-002 premise, live |
| Cache hit ratio by layer | `cache_hit_total`, `cache_miss_total` | |
| Breaker state and bulkhead in-flight by source | gauges | protection posture |
| Degraded / partial ratio | `stale_response_total`, `partial_response_total` | quality of service |
| Precedence conflicts by `field_group` | `precedence_conflict_total` | data-quality |
| Per-tenant top-N request rate and error rate | filtered by `tenant_id` | noisy-neighbour triage |

---

## 10. Export and reliability

| Aspect | Behaviour | Req |
|---|---|---|
| Trace export | OTLP/gRPC to `observability.otlp.endpoint`, batched | REQ-OBS-009 |
| Metric export | OTLP push at `observability.metrics_interval`, plus Prometheus scrape on `/metrics` (admin port only) | REQ-API-002 |
| Exporter failure | logged rate-limited; **never** blocks or fails a request; batch queue is bounded and drops oldest on overflow | REQ-OBS-009 |
| Resource attributes | `service.name`, `service.version`, `deployment.environment`, `k8s.pod.name`, `k8s.namespace.name`, `host.name` | |
| Shutdown | providers flushed within `server.shutdown_grace` after listeners drain, so the last requests' telemetry is not lost | REQ-RES-010 |
| Clock | a single injected `Clock` interface is used for all durations, so tests are deterministic | |

**Verified by** `TestObservability_ExporterFailureIsolated`,
`TestTracing_SpanTreeShape`, `TestTracing_ErrorRecording`,
`TestTracing_SamplerConfiguration`, `TestLogging_RequestLineSchema`,
`TestMetrics_InstrumentInventory`, `TestMetrics_TTLHitMissSemantics`,
`TestMetrics_NoHighCardinalityAttributes`, `TestMetrics_ExemplarsAttached`,
`TestFreshness_AgeHistogramRecorded`, `TestTracePropagation_InboundOutbound`.
