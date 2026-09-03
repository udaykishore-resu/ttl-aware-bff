# TTL-Aware BFF — Runbook

Operational procedures for `ttl-aware-bff`. Written for someone who did not build
it and is looking at a page at 3am.

The service's defining property, and the one that makes it diagnosable: **every
response carries the id of the rule that produced it**, and those rule ids are
frozen strings that appear identically in `meta.routingRule`, in
`routing_decision_total{routing_rule=...}`, in span attributes and in log lines.
Almost every question below is answered by grouping something by `routing_rule`.

| | |
|---|---|
| Namespace / workload | `bff` / `ttl-aware-bff` |
| Public API | `:8080`, behind the ALB |
| Admin (unauthenticated, cluster-internal only) | `:9090` |
| Dependencies | Operational Data Source (gRPC), Execution Data Source (REST), Redis (ElastiCache) |
| Config | `configs/bff.yaml`, ConfigMap `ttl-aware-bff`, hot-reloaded on a 15s watch |

---

## 1. The first three things, every time

Whatever the alert, start here. It takes under a minute and it decides which
section you read next.

```bash
# 1. Is it the BFF, or is it a source?
kubectl -n bff exec deploy/ttl-aware-bff -- \
  wget -qO- http://127.0.0.1:9090/readyz | jq
```

`readyz` reports both sources' state as the BFF sees it. It returns `200` even
when both are down — deliberately, because the BFF can still serve stale cached
data and removing every pod from the load balancer would turn a degraded service
into no service. Read the body, not the status code:

```jsonc
{ "status": "degraded",
  "sources": {
    "operational": { "available": false, "state": "CIRCUIT_OPEN" },
    "execution":   { "available": true,  "state": "HEALTHY" } } }
```

`state` is one of `HEALTHY`, `CIRCUIT_OPEN`, `CIRCUIT_HALF_OPEN`, `SATURATED`,
`UNCONFIGURED`.

```bash
# 2. What is the routing actually doing right now?
sum by (routing_rule, routing_decision) (rate(routing_decision_total[5m]))
```

This one query distinguishes almost every incident class. See §3 for what each
rule id means when it dominates.

```bash
# 3. What configuration is this pod actually running?
kubectl -n bff exec deploy/ttl-aware-bff -- \
  wget -qO- 'http://127.0.0.1:9090/config/routing?tenant=<tenant>' | jq
```

If `reload_failures` is non-zero, a config change was rejected and the pod is
running the previous configuration. That is a lead, not a coincidence.

---

## 2. Alerts

Each entry says what the alert means, what it does **not** mean, and where to go
next.

### `BFFHighErrorRate` — 5xx rate above the SLO

**Means.** Requests are failing outright, not degrading. Split them first:

```promql
sum by (http_status) (rate(bff_request_total{http_status=~"5.."}[5m]))
```

| Dominant status | Meaning | Go to |
|---|---|---|
| `503` with `NO_SOURCE_AVAILABLE` | The degradation ladder ran out of rungs | §4, §6 |
| `503` with `UPSTREAM_UNAVAILABLE` | A required source is down and has no substitute | §6 |
| `504` | A required source exceeded its budget | §6, §8 |
| `502` | A source returned something unusable | §9 |
| `500` | A real defect. This is the only one that is always a bug | escalate |

**Does not mean** the service is degrading. Degradation is `200` with
`degraded: true`, or `206` with `partial: true`. Those never appear here.

### `BFFStaleResponsesElevated` — `stale_response_total` rising

**Means.** The bottom rung of the ladder is carrying traffic: every source was
unavailable for those requests and the answer came from an expired cache entry.
The users are getting answers, and the answers are old.

```promql
sum by (request_type) (rate(stale_response_total[5m]))
```

This is a *source* incident, not a BFF incident. Go to §6. Note that tenants with
`allow_stale: false` (`globex` in the shipped configuration) are getting `503`s
for the same conditions, so a stale-serve alert and an error-rate alert can fire
together for one root cause.

### `BFFExecutionFallbackHigh` — `execution_fallback_total` rising

**Means.** Traffic that should be served by the fast source is going to the slow
one. Latency will follow. See §5, which is the most common routing surprise and
has its own decision tree.

### `BFFTTLMissRatioHigh` — operational data failing its freshness check

```promql
sum(rate(operational_ttl_miss_total[5m]))
  / (sum(rate(operational_ttl_hit_total[5m])) + sum(rate(operational_ttl_miss_total[5m])))
```

**Means** one of exactly three things, and §5 distinguishes them: the operational
source's data really is old, its freshness probe is failing, or somebody changed
a TTL.

### `BFFCircuitBreakerOpen` — `circuit_breaker_state == 2`

**Means.** The BFF has stopped calling a source. `0` closed, `1` half-open,
`2` open.

```promql
circuit_breaker_state
sum by (name, from, to) (increase(circuit_breaker_transition_total[15m]))
```

A breaker that opens and closes repeatedly (flapping) is a different problem from
one that is stuck open — see §6.3.

### `BFFPartialResponsesElevated` — `partial_response_total` rising

**Means.** `/details` is returning `206` with a field group missing, because an
optional source failed or timed out. The endpoint is doing exactly what it is
configured to do. The question is why the execution source is failing — §6 — and
whether the UI handles `206`.

### `BFFLatencyBudgetBreach` — p95/p99 out of budget

Budgets from REQ-PERF-001: `status`/`read`/`configuration` 60/120 ms;
`executions`/`execution_status` 250/500 ms; `details` 280/550 ms.

**Check the routing mix before the latency.** If traffic moved from the
operational source to the execution source, the latency change is a *consequence*
of a routing change, and chasing it as a performance problem wastes the incident.

```promql
sum by (routing_decision) (rate(routing_decision_total[5m]))
```

If the mix is unchanged, go to §8.

### `BFFPrecedenceConflictsElevated` — the two sources disagree

```promql
sum by (field) (rate(precedence_conflict_total[5m]))
```

**Means.** Both sources supplied a field with different values and the precedence
policy had to choose. A low background rate is normal during workflow activity.
A step change means one source's data diverged — see §10.

### `BFFBulkheadRejections` — `bulkhead_rejected_total` rising

**Means.** A source's concurrency allowance is exhausted (`operational: 256`,
`execution: 64`). This is the mechanism that stops a slow source from consuming
the whole BFF, so it firing means it is *working* — but it also means requests
are being refused. Check `bulkhead_in_flight` and the source's latency histogram:
a saturated bulkhead with normal source latency means the offered load grew; with
elevated source latency it means the source slowed down.

### `BFFConfigReloadFailing` — `reload_failures` increasing

**Means.** A configuration change was rejected by validation and the pod is
running the previous configuration. The service is fine; the change did not land.
See §11.

---

## 3. Reading `routing_decision_total`

This is the primary diagnostic instrument. Both attributes matter:
`routing_decision` is *where the request went*, `routing_rule` is *why*.

```promql
sum by (routing_rule, routing_decision) (rate(routing_decision_total[5m]))
```

| Dominant rule | What it means | Normal? |
|---|---|---|
| `ttl.operational.fresh` | The operational copy was inside its TTL; the slow source was never contacted | **Yes — this is the healthy steady state** |
| `fields.operational_only` | Rule 4 fired **and terminated**, which it does only when the operational source is unavailable. The decision is `NONE`, so the request went to the ladder. On the healthy path rule 4 pins the source and falls through, so `/configuration` reports a `ttl.*` rule instead | No → §6.2 |
| `fields.execution_only` | Required fields can only come from the execution source (`/executions`) | Yes, for those endpoints |
| `fields.span_both` | Required fields span both sources (`/details`) | Yes, for that endpoint |
| `ttl.operational.stale` | The operational copy was past its TTL and traffic was routed to the execution source | Occasionally. Sustained → §5 |
| `ttl.unknown_freshness` | Freshness could not be established at all | Rarely. Sustained → §5.2 |
| `health.primary_unavailable` | The preferred source was already known unhealthy at routing time | No → §6 |
| `fallback.primary_failed` | The preferred source failed *during the call* | No → §6.1 |
| `health.both_unavailable` | Neither source was usable | No → §6 |
| `degrade.stale_cache` | Answered from an expired cache entry | No → §6 |
| `consistency.strong_requires_operational` | A caller asked for `?consistency=strong` | Only if a client is sending it |
| `guard.tenant_missing` | A request arrived with no resolved tenant | No → §12 |
| `default.preferred_source` | The chain terminator fired | **Never, with the shipped config.** All six request types are caught earlier. Seeing this means a request type was added without freshness semantics, or the field catalogue changed. |
| `guard.unconfigured_request_type` | A request reached routing for a request type that has **no routing rule at all**. This is a pre-chain exit, not a rule: the configuration is broken, not the routing | **Never.** Alert on any non-zero rate → §11 |

Two rules deserve emphasis because they are easy to confuse:

- `health.primary_unavailable` is **pre-flight**: the circuit breaker already knew
  the source was unwell, so no call was issued.
- `fallback.primary_failed` is **post-dispatch**: the breaker did not know yet, a
  call was issued, it failed, and the application layer retried against the
  configured fallback.

Both mark the response `degraded: true` with a `SOURCE_UNAVAILABLE` warning, so
you cannot tell them apart from the `degraded` flag alone — only from the rule id.

A transition from the second to the first over a couple of minutes is the normal
signature of an outage as the breaker learns. The reverse — `fallback.primary_failed`
appearing *after* the breaker closed — means the source is flapping.

---

## 4. Reading `operational_ttl_miss_total`

TTL hit and miss are counted at the point of the freshness verdict, before any
source call.

```promql
# miss ratio
sum by (request_type) (rate(operational_ttl_miss_total[5m]))
  / ( sum by (request_type) (rate(operational_ttl_hit_total[5m]))
    + sum by (request_type) (rate(operational_ttl_miss_total[5m])) )
```

Read it together with the age distribution of what was actually served:

```promql
histogram_quantile(0.95, sum by (le, request_type) (rate(data_freshness_age_bucket[5m])))
```

| Miss ratio | `data_freshness_age` p95 | Diagnosis |
|---|---|---|
| High | High, near or above the TTL | The operational source's data genuinely is old. Its refresh pipeline is behind. |
| High | Low or unchanged | The data is fine; the *verdict* is wrong. Suspect a TTL change (§11) or clock skew (§5.3). |
| High | No data | The probe is failing, so no age is being recorded. §5.2. |
| Normal | High | Stale data is being served rather than routed away from — check `stale_response_total` and whether a `max_stale` is too generous. |

**A miss is not an error.** It is a route change. It becomes a user-visible
problem only through latency (the execution source is slower) or through
degradation (when there is no fallback).

---

## 5. "Why is traffic going to the slow source?"

The most common routing surprise. Work down this list; each step rules out a
cause.

### 5.1 Is it a TTL miss, or a health event?

```promql
sum by (routing_rule) (rate(routing_decision_total[5m]))
```

- `ttl.operational.stale` dominant → the data is old, or believed to be. Continue
  at 5.2.
- `health.primary_unavailable` or `fallback.primary_failed` dominant → the source
  is failing, not stale. Go to §6.
- `ttl.unknown_freshness` dominant → the probe is failing. Continue at 5.2.

### 5.2 Is the freshness probe working?

The probe is a separate, deliberately cheap RPC with its own budget
(`freshness_probe_timeout: 120ms`, against a `call_timeout` of `400ms`). If it
fails, the verdict is `UNKNOWN` and rule 10 applies
`routing.defaults.on_unknown_freshness` (default `operational`).

Three things about rule 10 are worth knowing before you read its output:
`none` is **honoured** — an operator who wrote it chose to fail rather than guess,
and gets `503 NO_SOURCE_AVAILABLE`; only an *unparseable* value silently falls
back to `operational`; and when a field-requirement rule pinned the source set
(`/configuration`, `/executions`), rule 10 may not cross the pin to the other
source, so an unavailable pinned source is `NONE` rather than a redirect.

```promql
# probe latency and errors, isolated from full reads
histogram_quantile(0.95,
  sum by (le) (rate(operational_source_latency_bucket{operation=~".*[Ff]reshness.*"}[5m])))
sum by (error_code) (rate(datasource_error_total{source="OPERATIONAL"}[5m]))
```

A p95 approaching 120 ms means the probe is timing out, which converts every
request into an unknown verdict. If the probe is slower than a full read, the
economic premise of the design is broken and the operational source's owners need
to know: the probe is supposed to be an index lookup on a freshness column, not a
hydrate of the whole record.

### 5.3 Is it clock skew?

Age is computed inside the source's own clock domain (`server_time − last_updated`),
which is immune to any offset between the BFF and the source. The two-clock
fallback path is used only when the source omits `server_time`, and it is biased
toward judging data *older*, never fresher.

```promql
sum(rate(clock_skew_detected_total[5m]))
```

Non-zero with a rising TTL miss ratio means the source's clock moved. Look at
`meta.freshness.skewCorrected` in a sample response and at
`routing.defaults.clock_skew_tolerance` (`2s`).

### 5.4 Did somebody change a TTL?

```bash
kubectl -n bff exec deploy/ttl-aware-bff -- \
  wget -qO- 'http://127.0.0.1:9090/config/routing?tenant=<tenant>' | jq '.request_types'
```

Compare `ttl` against the shipped values (`resource_status` 10s,
`resource_configuration` 30s, `resource_read` 30s, `execution_status` 5s,
`execution_history` 0s, `resource_details` 30s) and against the tenant overlay.
A tightened TTL raises the miss ratio by construction — that is what it is for —
and moves traffic to the slow source.

Check `reload_count` too: if it moved recently, something changed.

### 5.5 Is it one tenant?

```promql
topk(5, sum by (tenant_id) (rate(operational_ttl_miss_total[5m])))
```

`acme` runs a 5s status TTL and will always show a higher miss ratio than the
10s default. That is configured behaviour, not an incident.

---

## 6. A source is degraded

### 6.1 Execution source degraded or down

**Effect by endpoint** — this is the whole point of the routing configuration:

| Endpoint | `required_sources` | Result |
|---|---|---|
| `/status`, `/configuration` | operational only | **Unaffected** while operational data is fresh — the execution source is not on the path |
| `/details` | execution optional | `206` with `partial: true` and a `SOURCE_UNAVAILABLE` or `SOURCE_TIMEOUT` warning |
| `/executions`, `/executions/{id}` | execution required, `fallback: none` | `503 UPSTREAM_UNAVAILABLE`. No operational substitution is possible: the operational source structurally cannot supply execution history |
| `/status` **when operational data is stale** | falls back to execution | Now affected: the fallback target is the source that is down |

**Actions.**

1. Confirm the blast radius: `sum by (request_type, http_status) (rate(bff_request_total[5m]))`.
2. Confirm the BFF is protecting itself: `circuit_breaker_state{name=~".*execution.*"}`
   should reach `2` (open). If it has not, the source is failing slowly rather
   than cleanly — check `bulkhead_in_flight` for the execution source, which is
   capped at 64 precisely so a slow execution source cannot consume the BFF.
3. If `/details` latency is elevated rather than partial, the source is slow but
   not failing. `per_source_timeout.execution` is `1500ms`; requests should be
   bounded by that, not by the source. If they are not, the timeout is not being
   applied — escalate.
4. Nothing to change on the BFF. It is already degrading as designed. Do not
   raise `execution.call_timeout` to "give it a chance": that converts a fast
   partial answer into a slow one.

### 6.2 Operational source degraded or down

Higher impact: it is the preferred source for four of the six request types.

**Expected sequence, and it is visible in the rule ids:**

1. First failures → `fallback.primary_failed`, one call attempted per request,
   `degraded: true`, warning `SOURCE_UNAVAILABLE` **naming OPERATIONAL** — the
   warning names the source that failed, not the one that answered.
2. Once the failure ratio crosses `0.5` over `minimum_requests: 20` in the 30s
   window, the breaker opens → `health.primary_unavailable`, no calls attempted.
3. `/configuration` has `fallback: none`, so it goes cache → stale cache → `503`.
4. If the execution source also fails → `health.both_unavailable` → `degrade.stale_cache`
   or `503`.

**Expect the status codes to split by endpoint, and do not read the `206`s as
errors.** A fallback answer is a `206` whenever the execution source cannot hold
every field the endpoint promised:

| Endpoint | Fallback result |
|---|---|
| `/status` | `200` degraded. `status` lists the execution source as a supplier, so the answer is complete |
| `/resources/{id}` | **`206`** degraded + partial, with a `PARTIAL_DATA` warning: the EDS holds no `configuration`, `owner`, `metrics` or `topology` |
| `/configuration` | `fallback: none` — cache → stale cache → `503` |
| `/details` | the operational branch is the required one, and `fallback: operational` equals the primary, so no call-time fallback is possible: straight to the stale ladder |

A rise in `206`s during an operational outage is therefore the design working. If
you see `200`s where the table says `206`, the partial accounting is broken and
callers are being told an incomplete body is complete — escalate.

**Actions.**

1. Confirm the breaker opened. If it did not, requests are still paying a full
   timeout each — check whether the source is returning errors the breaker treats
   as *not evidence about its health* and therefore **abstains** on:
   `NOT_FOUND`, `INVALID_REQUEST`, `FORBIDDEN`, `UNAUTHENTICATED`,
   `TENANT_MISMATCH` and `SCHEMA_VERSION_MISMATCH`. The first five are the
   caller's fault. The last is deliberate for a different reason — see §9.

   Abstain means `Breaker.Do` records **neither** a success nor a failure. That
   matters most in half-open: recording them as successes would let a source
   answering nothing but 404s while genuinely down meet `half_open_successes` and
   be re-admitted to full traffic. So a breaker that will not open on a flood of
   client faults is working correctly — but a breaker stuck in half-open with no
   counted outcomes means the source is only producing client faults, and the
   window is not moving. Look at the source, not at the breaker.
2. Expect `stale_response_total` to rise for `allow_stale` request types and
   `503`s for the rest. Both are correct.
3. The stale window is finite: `cache.stale_grace` is `5m`, and `max_stale` is
   `120s` for `resource_status`, `300s` for the others. Beyond that even stale
   answers stop. Plan the incident around those numbers.

### 6.3 A flapping breaker

`increase(circuit_breaker_transition_total[15m])` high with the state oscillating
between `2` and `1` means the source recovers just enough to pass two half-open
probes and then fails again. Half-open admits at most 3 concurrent calls and
requires 2 consecutive successes, so flapping means the source is genuinely
intermittent rather than the breaker being twitchy. If it must be damped, raise
`breaker.open_timeout` (5s) or `breaker.half_open_successes` (2) — but treat that
as suppressing a symptom.

### 6.4 Redis unavailable

Caching fails **open**: `cache.fail_open: true` means a backend error is reported
to the caller as a cache miss and never fails a request.

Consequences: every request becomes a miss, source load rises by the previous hit
ratio, latency rises to the source-bound profile, and — the one that bites — the
stale-serve rung disappears, because it reads through the same cache. A Redis
outage during a source outage removes the last rung of the ladder.

Watch `cache_error_total` and `cache_hit_total{layer=...}`. The in-process L1
still functions in the `layered` backend; with `backend: redis` there is no L1 to
fall back on.

`cache_error_total` also counts **background-refresh** failures
(`op="refresh"`). An early refresh fires at 80% of an entry's life, runs on a
context that keeps the request's values but not its cancellation, and is bounded
by a fixed 10s budget rather than by the cache TTL. A rise there with a healthy
foreground path usually means the sources are slow enough that refreshes are
timing out — which is a warning that the expiry cliff is about to start landing on
user requests.

---

## 7. Using `/config/routing`

The single most useful admin endpoint during a routing incident: it answers "what
is this pod *actually* using", including the tenant overlay, without reading YAML
or guessing which ConfigMap version landed.

```bash
# Effective policy for one tenant
kubectl -n bff exec deploy/ttl-aware-bff -- \
  wget -qO- 'http://127.0.0.1:9090/config/routing?tenant=acme' | jq

# Just one request type
... | jq '.request_types.resource_status'

# Defaults, including the ones that decide degradation behaviour
... | jq '.defaults'
```

```jsonc
{
  "tenant": "acme",
  "request_types": {
    "resource_status": {
      "preferred_source": "operational",
      "ttl": "5s",            // the acme overlay; the base value is 10s
      "cache_ttl": "2s",
      "fallback": "execution",
      "allow_stale": true,
      "max_stale": "1m0s",
      "consistency": "bounded"
    }
  },
  "defaults": { "on_unknown_freshness": "operational", "clock_skew_tolerance": "2s", ... },
  "reload_count": 3,
  "reload_failures": 0
}
```

**How to use it.**

- **Confirm an overlay applied.** Query with and without `?tenant=`. If they are
  identical, the overlay did not land.
- **Confirm a rollout reached every pod.** Run it against several pods by name,
  not against the Service. Config is reloaded per pod on a 15s watch, so a
  ConfigMap update propagates unevenly for a few seconds — and a pod whose reload
  failed validation keeps its previous configuration indefinitely.
- **Check `reload_failures`.** Non-zero means a change was rejected. The pod is
  running the last good configuration; the logs carry the validation error.
- **Sanity-check the invariant.** `cache_ttl` must be ≤ `ttl` for every request
  type. Validation enforces it, so a violation here would mean a validation gap —
  but it is a cheap thing to eyeball when a freshness result looks impossible.

The endpoint is read-only and redacted. It is on the admin listener, which is
unauthenticated by design and restricted to the cluster by the NetworkPolicy;
never expose it through the ingress.

---

## 8. Latency investigation

Work outside in.

```promql
# 1. Where is the time? Client-observed, by route class.
histogram_quantile(0.95, sum by (le, request_type) (rate(bff_request_latency_bucket[5m])))

# 2. How much of it is a source?
histogram_quantile(0.95, sum by (le) (rate(operational_source_latency_bucket[5m])))
histogram_quantile(0.95, sum by (le) (rate(execution_source_latency_bucket[5m])))

# 3. For /details, is the fan-out costing max() or sum()?
histogram_quantile(0.95, sum by (le) (rate(aggregation_latency_bucket[5m])))
```

| Pattern | Diagnosis |
|---|---|
| Request latency up, source latency flat, routing mix unchanged | BFF-side. Check `bff_concurrent_requests`, GC, CPU throttling, `bulkhead_in_flight`. |
| Request latency up, routing mix shifted toward `EXECUTION` | Not a latency problem. §5. |
| Source latency up for one source | Source-side. §6. |
| `aggregation_latency` ≈ operational + execution rather than ≈ max | The fan-out is serialising. That is a defect — the branches are supposed to be concurrent with independent contexts (REQ-PERF-006). Escalate. |
| Cache hit ratio dropped | §6.4, or the working set changed shape. |

The server span is named by route *pattern* (`GET /api/v1/resources/{resourceId}/status`),
never by path. Inside it the service emits exactly five spans:
`bff.usecase.resource` / `bff.usecase.execution`, `bff.route`, `bff.aggregate`
and — only when the operational record declared an in-flight execution —
`bff.resolve_in_flight`. There is no per-source-call, per-mapper or per-precedence
span; those are metrics (`operational_source_latency`, `execution_source_latency`,
`precedence_conflict_total`).

If `/status` latency rose and `bff.resolve_in_flight` is present on those traces,
a workflow is running on the resources being polled and each request is paying an
extra execution-source call bounded by `in_flight_lookup_timeout` (300ms).

---

## 9. A source returned something unusable

`502 UPSTREAM_INVALID_RESPONSE` or `502 SCHEMA_VERSION_MISMATCH`.

```promql
sum by (source, error_code) (rate(datasource_error_total[5m]))
sum by (source) (rate(schema_version_mismatch_total[5m]))
```

- A **minor** schema version ahead of what the BFF accepts is tolerated: unknown
  fields ignored, unknown enum members mapped to `UNKNOWN`, warning
  `SCHEMA_VERSION_MISMATCH` on the response.
- A **major** mismatch is fatal *for that call*. Accepted versions are
  `sources.<source>.accepted_schema_versions` (`ods.v1`, `eds.v1`).
- Malformed payloads are **not retried** — retrying a deterministic parse failure
  wastes the caller's budget. If the source is optional, the request continues as
  partial.

**A schema mismatch does not trip the circuit breaker, and you should not expect
it to.** `resilience.isClientFault` lists `SCHEMA_VERSION_MISMATCH` alongside the
caller-fault codes, so the breaker records it as a success and the source stays
`HEALTHY` in `/readyz` and in `circuit_breaker_state`. That is deliberate: the
source is up, fast and answering correctly, and it is the BFF that does not
understand the contract. Opening the circuit would trip a breaker on a healthy
source, silently reroute every request to the slower one, and replace a loud,
specific version-incompatibility alert with a vague availability one.

The mismatch surfaces in three places instead:

| Where | What you see |
|---|---|
| `schema_version_mismatch_total` | The metric that should page. Alert on any non-zero rate |
| `errs.SourceUnusable` | True for `SCHEMA_VERSION_MISMATCH`, so the **call-time fallback** (`fallback.primary_failed`) runs where the request type has one, and the request succeeds `200` degraded |
| The response | `502 SCHEMA_VERSION_MISMATCH` where no fallback is configured — `/configuration`, `/executions`, `/executions/{id}` |

So during a contract break, expect `/status` and `/resources/{id}` to keep working
via `fallback.primary_failed` while `/configuration` returns `502`, with the
breakers all showing green. That combination is the signature; do not read the
green breakers as "the BFF is not noticing".

Offending payloads are logged truncated and redacted, never returned to the
client. Find them by correlation id in the pod logs.

---

## 10. Sources disagree

`precedence_conflict_total` rising, or a user reporting that `/status` and
`/details` show different things.

**How precedence works, so you can predict the answer.** The configured per-field
order in `precedence.fields` decides, most authoritative first — and *recency is
never a tiebreaker*. The execution source can legitimately hold a newer timestamp
for a status it is only predicting from a workflow, while the operational source
holds the older but observed truth. The single exception is
`execution_overrides_when_running: [status, subState]`, which applies only while a
workflow is actually in progress.

```promql
sum by (field) (rate(precedence_conflict_total[5m]))
```

Inspect one response: `meta.provenance` names the winning source per field, and a
`CONFLICT_RESOLVED` warning names the loser.

**If `/status` and `/details` disagree**, check `routing.defaults.resolve_in_flight_execution`
is `true`. That setting is what makes `/status` — an operational-only read —
consult the execution source when the operational record declares an
`in_flight_execution_ref`, so the override rule can fire on both endpoints. It is
best-effort: if the lookup fails or times out (300 ms), the operational answer
stands and the two endpoints can diverge for the duration. Look for
`bff.resolve_in_flight` spans with a non-OK outcome and for the debug log
"in-flight execution lookup failed; serving the operational answer".

---

## 11. Configuration changes

Everything routing-related is configuration; nothing under `internal/` compiles in
a TTL, timeout or threshold.

### Changing a TTL

1. Decide which one. `ttl` changes *routing* — how old the source's observation
   may be. `cache_ttl` changes only how long the BFF reuses its own answer.
   Confusing them is the most common mistake with this service.
2. `cache_ttl` must be ≤ `ttl`. Validation rejects the reverse, because a cache
   entry that outlives the freshness verdict would serve data the policy already
   calls stale.
3. Edit the ConfigMap (or the Helm values), apply, and wait one watch interval
   (15s).
4. Verify on a pod with `/config/routing`, and confirm `reload_failures` did not
   move.
5. Watch `operational_ttl_miss_total` and `execution_fallback_total`: a tighter
   TTL raises both by construction and shifts latency toward the execution
   source. That is the trade being made deliberately.

For an urgent single-value change, the environment override works without editing
YAML — but it requires a pod restart, so prefer the ConfigMap path:

```
BFF_ROUTING__REQUEST_TYPES__RESOURCE_STATUS__TTL=15s
```

### A request type with no routing rule

`guard.unconfigured_request_type` in `routing_decision_total` means a request
reached routing for a type that has no entry under `routing.request_types` at
all — not a routing outcome, a broken configuration. Every such request is a
`503 NO_SOURCE_AVAILABLE`.

It is emitted to the metric rather than only logged precisely so this is visible
in the one query you already run. Compare `/config/routing` against the six
shipped request types; the usual cause is a partial ConfigMap that dropped a
block, or an environment override that replaced a map instead of merging into it.

### A rejected reload

`reload_failures > 0` means validation refused the new configuration and the
previous one is still in force. The service is healthy. Find the error in the pod
logs (the config reload logs the validation failure), fix the ConfigMap, reapply.
Common causes: `cache_ttl > ttl`, `freshness_probe_timeout ≥ call_timeout`,
`allow_stale: true` combined with `consistency: strong`, and
`security.allow_insecure_no_auth: true` outside a `local`/`test` environment.

### Emergency: force traffic off the operational source

There is no kill switch. The supported way is to make the request type prefer the
other source:

```
BFF_ROUTING__REQUEST_TYPES__RESOURCE_STATUS__PREFERRED_SOURCE=execution
```

Understand the cost before doing it: the execution source cannot supply
`configuration`, `metrics`, `topology` or `owner`, so endpoints needing those will
degrade to partial or fail. Prefer letting the breaker do its job.

---

## 12. Tenant issues

| Symptom | Cause | Action |
|---|---|---|
| `400 INVALID_REQUEST`, rule `guard.tenant_missing` | The request reached routing with no resolved tenant. The BFF fails closed: there is no safe default tenant | Check the auth middleware and the token's `tenant_id` claim. A spike here after an auth change is the signal that the claim stopped being populated |
| `403 TENANT_MISMATCH` | `X-Tenant-ID` disagrees with the token's tenant. Refused rather than resolved in either direction | Client bug, or someone probing. Check the audit log |
| One tenant sees different TTLs | Working as configured | `/config/routing?tenant=<t>` shows the overlay |
| One tenant gets `503` where others get degraded `200` | That tenant has `allow_stale: false` (`globex` in the shipped config) | Working as configured. Changing it is a policy decision, not an incident fix |
| `429 RATE_LIMITED` for one tenant | Per-tenant limit: `rps: 200`, `burst: 400`, `acme` 500/1000 | `rate_limited_total` by tenant. Raise the overlay if legitimate |

Cache keys are tenant-scoped, so no cache manipulation can cross tenants.

---

## 13. Cache invalidation

**Before invalidating, be sure the cache is the problem.** Cache TTLs are short —
`resource_status` 3s, `resource_read` 5s, `resource_details` 5s,
`resource_configuration` 15s, execution history not cached at all. `/executions/{id}`
has `cache_ttl: 2s` but is configured `consistency: strong`, so it is written to the
cache and **never read from it**: `meta.cache.hit` is always `false` there, and
invalidating it changes nothing a caller can see.

A **degraded or partial** entry is clamped to `negative_ttl` (3s) when stored, so
neither can outlive the incident that produced it — another reason a stale-looking
answer older than a few seconds is usually not a cache problem. Anything that
has persisted for more than a minute is almost certainly not a stale cache entry.
A cache hit re-reports the *aged* freshness, so a stale cached answer arrives
marked `degraded: true` with `freshness.state: STALE` — check the envelope before
reaching for the cache. One exception to watch for: an entry stored with
`state: UNKNOWN` stays `UNKNOWN` however long it sits there. `EffectiveFreshness`
re-derives `FRESH`/`STALE` from the accumulated age only when the stored verdict
was one of those two, because the router treats `UNKNOWN` and `STALE` differently
and a cache hit must not manufacture a verdict the original read never produced.

### Order of preference

1. **Wait.** Under a minute in every case. This is usually correct.
2. **Invalidate one resource.** `Service.InvalidateResource` drops every cached
   view of one resource across all request types. Note that no HTTP admin route
   currently exposes it — it is called from tests and is the hook a change
   notification would use. Use step 3 or 4 in production today.
3. **Invalidate one tenant.** `Manager.InvalidateTenant` deletes by the tenant key
   prefix returned by `cache.TenantPrefix`.
4. **Delete keys directly in Redis.** Get the layout right first — it is not the
   obvious one.

### The key layout

```
<key_prefix>:e<entrySchemaVersion>:t=<tenant>:rt=<requestType>:r=<resource>[:s=<sub>][:v=<variantHash>]
```

With the shipped `key_prefix: bff:v1` and `EntrySchemaVersion = 1`:

```
bff:v1:e1:t=acme:rt=resource_status:r=R042
bff:v1:e1:t=acme:rt=execution_history:r=R042:v=9f2c11a4b7e05d38
```

Four things about it are load-bearing:

- **Every segment is labelled** (`t=`, `rt=`, `r=`, `s=`, `v=`). A bare
  colon-joined key would let a resource id impersonate a request type.
- **The `e1` segment is the entry schema version.** Bumping `cache.EntrySchemaVersion`
  changes every key, which is how a rolling deploy that changes the payload shape
  stays safe without flushing Redis. Entries under the old prefix simply go unread
  and expire.
- **The tenant comes first**, so a tenant flush is a single prefix delete.
- **`cache.TenantPrefix` ends with the delimiter** — `bff:v1:e1:t=acme:`, colon
  included. That trailing colon is not cosmetic: without it the prefix also
  matches `bff:v1:e1:t=acme2:…`, and flushing tenant `acme` would silently empty
  tenant `acme2` as well. If you are hand-writing a delete pattern, include it.

Segment values are sanitised: an empty value becomes `~` (a character outside the
safe set, so it cannot collide with a tenant literally named `-`), and a value
containing anything outside `[A-Za-z0-9._-]` collapses to `h_<24 hex chars>` — so
a key you cannot find by name may be a hashed id rather than a missing entry.

```bash
# Inspect before deleting. SCAN, never KEYS, on a production instance.
redis-cli --scan --pattern 'bff:v1:e1:t=acme:rt=resource_status:*' | head

# One resource, every view. Note the ':' after the tenant.
redis-cli --scan --pattern 'bff:v1:e1:t=acme:*:r=R042*' | xargs -r redis-cli DEL

# One tenant, everything — the same prefix Manager.InvalidateTenant uses
redis-cli --scan --pattern 'bff:v1:e1:t=acme:*' | xargs -r redis-cli DEL
```

### What invalidation does not reach

- **L1.** In the `layered` backend each replica holds its own in-process cache
  with a 2s TTL. Deleting from Redis does not evict L1; those entries expire on
  their own within 2 seconds.
- **The probe memo.** Freshness observations are memoised for
  `probe_cache_ttl: 1s`, separately from the response cache. It holds the
  *observation*, not the verdict, and the manager advances the memoised
  `SourceTime` by the time the entry has spent in the memo — so a reused probe
  result reports a larger age, never a fresher one, and the memo cannot be the
  cause of a record looking newer than it is.
- **Negative entries.** A `404` is cached for `negative_ttl: 3s`. If a resource
  was created moments ago and is still reported missing, that is why.

### The nuclear option

`redis-cli FLUSHDB` on the cache instance. Every replica takes a cold-start miss
simultaneously, so the sources see a burst. Singleflight and the Redis stampede
lock collapse concurrent misses per key, which bounds the burst — but do this
during low traffic, and only if you have ruled out everything else.

---

## 14. Deploy, rollback, capacity

### Health semantics — know these before touching a probe

| Probe | Fails when | Deliberately does **not** fail when |
|---|---|---|
| `/livez` | The process is unhealthy | A source is down. A dependency check here would turn a source blip into a cluster-wide restart storm |
| `/readyz` | The pod is still starting | A source is down. It reports `status: degraded` with `200`, because the BFF can still serve stale cache and removing every pod would turn a degraded service into no service |
| `/healthz` | Startup has not completed | — |

If you find yourself wanting readiness to fail during a source outage: it should
not. The stale-serve path is the reason.

### Rollout

Graceful shutdown arithmetic is fixed and must stay consistent: `preStop` sleep
(10s) + `server.shutdown_grace` (25s) < `terminationGracePeriodSeconds` (45s).
Changing one without the others causes in-flight requests to be cut off.

```bash
kubectl -n bff rollout status deploy/ttl-aware-bff
kubectl -n bff rollout undo   deploy/ttl-aware-bff      # rollback
```

After any rollout, confirm the routing mix is unchanged:

```promql
sum by (routing_rule) (rate(routing_decision_total[5m]))
```

A rule distribution that changed across a deploy, with no configuration change,
is the signal that the deploy altered behaviour.

### Capacity

HPA: 3–30 replicas at 70% CPU. Before scaling out, check whether the bottleneck
is actually the BFF: if `bulkhead_rejected_total` is rising for a source, more
replicas multiply the load on that source rather than relieving it. The bulkheads
(`operational: 256`, `execution: 64` per replica) are per-pod, so replica count
directly multiplies the concurrency a source sees.

---

## 15. Escalation

| Condition | Owner |
|---|---|
| `500`s | BFF team. Always a defect |
| `aggregation_latency` ≈ sum of branches rather than max | BFF team. The fan-out is serialising |
| `default.preferred_source` appearing in the rule mix | BFF team. The chain reached its terminator, which the shipped configuration should never do |
| `guard.unconfigured_request_type` appearing at all | BFF team. A request type has no routing rule; the deployment's configuration is incomplete |
| The freshness probe is not materially cheaper than a full read | Operational source team. This breaks the design's premise |
| Operational data persistently past its TTL with the probe healthy | Operational source team. Their refresh pipeline is behind |
| Execution source latency past its budget | Execution source team |
| `precedence_conflict_total` step change | Both source teams; the two systems' views diverged |

When escalating, include the correlation id, `meta.routingRule` and
`meta.routingDecision` from an affected response, and the output of
`/config/routing` for the tenant. Those three between them reconstruct what the
service did and why, without needing access to the source.
