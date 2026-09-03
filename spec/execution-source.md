# Execution Data Source (EDS)

The workflow / execution / history / audit source. REST over HTTPS, JSON.
Consumed by `internal/datasource/execution` behind `ExecutionPort`
(REQ-DS-001, REQ-DS-004).

Reference stub binary: `cmd/exsource` (HTTP `:9102`, admin/chaos `:9112`).

---

## 1. Characteristics

| Property | Value | Consequence for the BFF |
|---|---|---|
| Protocol | REST / HTTP1.1 or 2, `application/json` | connection pooling matters; no multiplexing guarantee |
| Consistency | authoritative for workflow state; write-through from the workflow engine | it is the **only** truth about a running execution |
| Latency profile | P50 ≈ 90 ms, P95 ≈ 240 ms, P99 ≈ 480 ms | roughly 5× the ODS; calling it unnecessarily is the primary cost regression |
| Data shape | execution-centric, deep records (steps, actions, result, audit) | responses are large; pagination is mandatory for history |
| Vocabulary | `status`, `customerId`, `phase`; multiple spellings per status | mapped through a synonym table (`spec/data-contracts.md` §6.2) |
| Tenancy | `tenantId` is a required query parameter and appears in every record | BFF asserts it independently (REQ-SEC-006) |
| Freshness | `observedAt` in the body; no dedicated probe RPC | there is no cheap freshness question to ask (§6) |
| Idempotency | all four endpoints are `GET` | retry always safe in principle (REQ-DS-008) |
| Schema version | body `schemaVersion`, else `X-Schema-Version` header | gated per REQ-EDGE-017 |

**Field groups only the EDS can supply**: `latestExecution`, `executionHistory`,
`lastOperation`, and — while an execution is `RUNNING` — the authoritative
`status` and `subState` (REQ-PREC-003).

---

## 2. Endpoints

### 2.1 `GET /eds/v1/executions?resourceId=&tenantId=&limit=`

List executions for a resource, newest first.

| Parameter | Required | Constraint |
|---|---|---|
| `resourceId` | yes | `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` |
| `tenantId` | yes | the authenticated tenant; never taken from a client header alone |
| `limit` | no | 1..200, default 50 |
| `cursor` | no | opaque, base64url, ≤ 512 B |

```jsonc
// 200
{
  "executions": [ /* ExecutionRecord, newest first by startedAt */ ],
  "nextCursor": "eyJvIjoxMDB9",
  "totalKnown": 143,
  "schemaVersion": "2.4.0",
  "observedAt": "2026-09-03T12:00:41.900Z"
}
```

Used by `execution_history` (`ttl: 0s`, `cache_ttl: 0s`, always live).
`executions: []` on an existing resource is an **empty collection**, not a
not-found: the BFF returns `200` with `data.executions: []` and warning
no warning (REQ-EDGE-019b). `totalKnown` is on the declared drop list — it is
unstable under concurrent writes and exposing it would imply a consistency
guarantee the EDS does not make.

### 2.2 `GET /eds/v1/executions/{executionId}`

Single execution by id.

```jsonc
// 200 → ExecutionRecord (see spec/data-contracts.md §3.2)
// 404 → the execution does not exist
```

Used by `execution_status` (`ttl: 5s`, `cache_ttl: 2s`, `allow_stale: false`).
The BFF additionally asserts `record.resourceId == requested resourceId`; a
mismatch is a `404`, not a cross-resource read.

### 2.3 `GET /eds/v1/resources/{resourceId}/latest-execution`

The most recent execution for a resource, or `404` when the resource has never
executed.

```jsonc
// 200 → ExecutionRecord
// 404 → no executions for this resource (NOT a request-level 404)
```

Used by the execution branch of `resource_details`. A `404` here maps to
`LatestExecution = nil` and, when the request also lacks any other execution
data, no warning (an empty collection is an answer) — it never fails the `/details` request.

**Why this endpoint exists rather than filtering the list endpoint.** It bounds
the response to one record and lets the EDS answer from an index rather than
paginating. Using `?limit=1` would still serialize the full record set's
envelope and would make the BFF's cost depend on EDS pagination internals.
It is also what keeps REQ-AGG-007 (no N+1) satisfiable: one call per resource,
never one call per execution.

**Why not fetch by `in_flight_execution_ref`.** The ODS's
`in_flight_execution_ref` is a *weak* reference that can dangle in both
directions (set after completion, unset during startup). Fetching by it would
turn a stale reference into a `404` on a healthy resource. The BFF always
resolves by `resourceId` (`spec/data-contracts.md` §10.3).

### 2.4 `GET /eds/v1/health`

```jsonc
{ "state": "SERVING", "detail": "", "schemaVersion": "2.4.0" }
```

`state` ∈ `SERVING | DEGRADED | NOT_SERVING`; anything else maps to `UNKNOWN` and
is treated as unavailable. Polled by the background health poller at a jittered
5 s interval and read from the snapshot by the router (REQ-DS-006). Never called
on the request path.

---

## 3. Request headers sent by the adapter

| Header | Value | Req |
|---|---|---|
| `Authorization` | service credential for the EDS (not the end-user JWT) | REQ-SEC-007 |
| `X-Correlation-ID` | the request's correlation id — the cross-protocol join with the ODS's `RequestContext.correlation_id` | REQ-API-010 |
| `X-Tenant-ID` | authenticated tenant, redundant with the `tenantId` query parameter (defence in depth) | REQ-SEC-004 |
| `X-Principal` | JWT subject, for the EDS's audit trail | REQ-SEC-012 |
| `traceparent` / `tracestate` | W3C propagation via `otelhttp` | REQ-API-011 |
| `Accept` | `application/json` | |
| `Accept-Encoding` | `gzip` | §5 |

The end-user JWT is **not** forwarded. The EDS is a downstream service with its
own credential; forwarding user tokens would make the EDS's authorization surface
depend on the BFF's token issuer and would leak user credentials into a system
that does not need them.

---

## 4. JSON payload contract

Full field tables are in `spec/data-contracts.md` §3 and §7. Summary of the parts
that drive BFF behaviour:

| Field | Role in the BFF |
|---|---|
| `executionId`, `resourceId`, `tenantId` | **mandatory**; absence ⇒ `UPSTREAM_INVALID_RESPONSE` (REQ-MAP-008); `tenantId` mismatch ⇒ `TENANT_MISMATCH` (REQ-SEC-006) |
| `status` | mapped through the synonym table; unknown ⇒ `UNKNOWN` + `SCHEMA_VERSION_MISMATCH` (REQ-MAP-004) |
| `status == RUNNING` | triggers the precedence override for `status` / `subState` (REQ-PREC-003) and clamps affected cache TTLs to ≤ 2 s (REQ-EDGE-015) |
| `observedAt` | the freshness observation for execution-sourced groups; absent ⇒ falls back to `updatedAt`; both absent ⇒ `UNKNOWN` |
| `completedAt: null` on a non-terminal status | **normal**, no warning (REQ-EDGE-007) |
| `result: null` on a terminal status | warning `PARTIAL_DATA` (REQ-EDGE-007) |
| `error.retryable` | **advisory only** — never drives BFF retry policy (REQ-RES-003) |
| `schemaVersion` | major mismatch ⇒ `SCHEMA_VERSION_MISMATCH`. The source is **not** marked unavailable and its breaker is **not** tripped — it is healthy, only unintelligible. `errs.SourceUnusable` is true, so the *call-time* fallback serves where one is configured, else `502` (REQ-EDGE-017) |
| `internalTraceId` | declared drop (`spec/data-contracts.md` §8) |

Timestamps are RFC 3339 with a `Z` offset. An unparseable timestamp on a
mandatory field is `UPSTREAM_INVALID_RESPONSE`; on an optional field it becomes a
zero time with a warning.

---

## 5. Pagination

| Aspect | Behaviour |
|---|---|
| Mechanism | opaque `cursor` returned as `nextCursor`; the BFF re-emits it verbatim as `data.nextCursor` |
| Ordering | newest first by `startedAt`, ties broken by `executionId` (REQ-API-012) — the BFF re-sorts defensively so response determinism does not depend on EDS ordering |
| Page size | client `limit` 1..200, default 50; out of range ⇒ `400 INVALID_REQUEST`, never clamped (REQ-API-014) |
| Cursor validation | base64url, ≤ 512 B (REQ-SEC-009); a malformed cursor is rejected before the upstream call |
| Cursor opacity | the BFF never decodes, re-encodes or synthesises a cursor; it is EDS-owned state |
| Multi-page fetching | **prohibited** on the request path. One inbound request produces at most one EDS list call. Assembling N pages server-side would make latency unbounded and defeat per-source deadlines (REQ-AGG-007). |
| Cursor stability | cursors are not guaranteed stable across EDS deploys; an EDS `400` on a stale cursor surfaces as `400 INVALID_REQUEST` with a `detail` instructing the client to restart pagination |
| Compression | `Accept-Encoding: gzip`; history pages are the largest bodies the BFF handles |
| Body bound | 4 MiB (REQ-DS-009); exceeding ⇒ `UPSTREAM_INVALID_RESPONSE`, not an OOM |

---

## 6. Latency profile and budget

Config subtree `sources.execution`:

```yaml
sources:
  execution:
    base_url: https://eds.internal
    call_timeout: 1500ms
    dial_timeout: 2s
    max_idle_conns_per_host: 32
    idle_conn_timeout: 90s
    tls:
      enabled: true
      ca_file: /etc/bff/tls/eds-ca.pem
      min_version: "1.3"
      server_name: eds.internal
    retry:    { max_attempts: 2, base_backoff: 50ms, max_backoff: 400ms, jitter: full }
    breaker:  { failure_ratio: 0.5, min_requests: 20, window: 30s, cooldown: 15s, half_open_successes: 3 }
    bulkhead: { max_concurrent: 32, acquire_timeout: 50ms }
```

| Phase | Key | Default | Notes |
|---|---|---|---|
| Dial | `dial_timeout` | 2s | amortised by the idle pool |
| Call | `call_timeout` | 1500ms | narrowed to fit the remaining request deadline (REQ-RT-006) |
| Per-attempt with retry | derived | `call_timeout / max_attempts` | `max_attempts: 2` — the EDS is slow enough that a third attempt rarely fits a budget |
| Bulkhead | `max_concurrent` | 32 | **half** the ODS bulkhead: a slow EDS must not consume the goroutine and connection budget that operational traffic needs (REQ-RES-007) |

Latency targets (REQ-PERF-001): `EXECUTION` P95 250 ms / P99 500 ms;
`BOTH` P95 280 ms / P99 550 ms. The `BOTH` figure is only ~30 ms above the
`EXECUTION` figure because the branches are concurrent — fan-out costs the slower
source, not the sum (REQ-PERF-006).

### 6.1 Why there is no freshness probe

The ODS has `GetResourceFreshness` because "how old is the current-state record"
is a cheap, meaningful question whose answer changes routing. The EDS has no
equivalent, for three reasons:

1. **There is no alternative source.** A freshness verdict is only actionable if
   it can send the request somewhere else. Nothing else can supply
   `executionHistory` or `latestExecution`, so a `STALE` verdict on the EDS has
   no available action other than serving it anyway.
2. **The relevant request types are configured live.** `execution_history` has
   `ttl: 0s` and `execution_status` has `allow_stale: false` — neither has a
   freshness branch to take.
3. **The record is the observation.** For a workflow execution, `updatedAt` *is*
   the state; there is no separate "when did we last refresh our copy" question,
   because the EDS is the system of record rather than a materialised view.

Freshness for execution-sourced groups is therefore derived from the body's
`observedAt` after the call, not probed before it.

---

## 7. Why the EDS must never be called when fresh operational data suffices

This is the economic premise of the service, and it is enforced structurally
rather than by convention.

### 7.1 The cost asymmetry

| | ODS | EDS | Ratio |
|---|---|---|---|
| P50 | 8 ms | 90 ms | 11× |
| P95 | 45 ms | 240 ms | 5.3× |
| P99 | 90 ms | 480 ms | 5.3× |
| Typical body | 2–40 KiB | 20–400 KiB (history) | ~10× |
| Bulkhead budget | 64 | 32 | EDS capacity is scarcer |

An unnecessary EDS call on a `resource_read` does not merely add latency to that
request. It consumes one of 32 concurrency permits, and at scale it shifts the
service's latency distribution from the ODS profile to the EDS profile — a
5× regression across the board that no amount of caching downstream can recover.

### 7.2 The correctness argument

When the operational record is fresh, the EDS cannot improve the answer for
operational field groups — it does not have them. Calling it would add
`latestExecution` data the request did not ask for. Paying 240 ms at P95 for data
outside `RequiredFields` is pure waste.

### 7.3 The enforcement points

Prevention is structural, not a code-review convention:

| Mechanism | What it prevents | Req |
|---|---|---|
| Rule 8 `ttl.operational.fresh` terminates the chain with `Target = OPERATIONAL` | the fresh path can never reach a rule that adds EXECUTION | REQ-RT-001, REQ-EDGE-001 |
| `routing.defaults.resolve_in_flight_execution` is the **one** documented exception | an operational-only read may contact the EDS, but only when the operational record itself declares an `in_flight_execution_ref`, under a 300ms budget, best-effort. The common case still pays nothing | REQ-PREC-003 |
| `Decision.Target` is the sole dispatch input to aggregation | a handler cannot "just also fetch" executions | REQ-RT-004 |
| Rule 3 fires only when required fields are execution-only | capability, not preference, is what selects the EDS | REQ-CLS-003 |
| `required_sources.execution: false` on `resource_details` | the EDS is never a hard dependency of the heavy endpoint | REQ-RT-007 |
| `TestEdge001_OperationalFresh` asserts the EDS fake recorded **zero** calls | regression is caught in CI, not in production | REQ-EDGE-001 |
| `execution_fallback_total` is a first-class metric with an SLO | an unexplained rise is alertable | REQ-OBS-001 |

### 7.4 The two legitimate reasons to call the EDS

1. **Capability** — a required field group can only come from the EDS
   (rules 3 and 5).
2. **Freshness failure** — the operational record is stale or unavailable and the
   EDS can supply the required fields (rules 7 and 9a).

Any other EDS call is a defect. In particular, calling the EDS "to enrich" a
fresh operational response, or "to double-check" a status, is prohibited: the
first is unrequested cost and the second is a distributed consistency check the
precedence policy already answers deterministically (REQ-PREC-002).

---

## 8. Connection management

| Setting | Value | Rationale |
|---|---|---|
| Shared `http.Client` + `http.Transport` | one process-wide | per-request clients leak connections and defeat pooling (REQ-DS-005) |
| `MaxIdleConnsPerHost` | 32 (= bulkhead width) | a pool narrower than the bulkhead forces handshakes under load; wider than it is dead weight |
| `IdleConnTimeout` | 90s | must be **below** any intermediary LB idle timeout, or the BFF reuses a connection the LB has already reaped and eats a spurious reset |
| `ForceAttemptHTTP2` | true | multiplexing when the EDS offers it; transparent fallback to HTTP/1.1 |
| `ExpectContinueTimeout` | 1s | irrelevant for GETs, set for correctness |
| `DisableCompression` | false | history bodies compress well |
| `ResponseHeaderTimeout` | not used | the per-call context deadline is the single source of truth for timeouts (REQ-RES-001) |
| Body handling | always fully read (bounded) and closed, even on error | an undrained body cannot be pooled |
| Body bound | 4 MiB via `io.LimitReader` | REQ-DS-009 |
| Middleware order | `otelhttp` → correlation/tenant headers → bulkhead → breaker → retry → per-attempt timeout | retry inside the breaker (one logical failure) and inside the bulkhead (one permit) |

---

## 9. Retry and error mapping

All four endpoints are `GET` and idempotent (REQ-DS-008); eligibility is still
decided by classification, never by "an error occurred" (REQ-RES-003).

| HTTP status / condition | `pkg/errs` code | Retryable | Breaker | Notes |
|---|---|---|---|---|
| `200` | — | — | success | |
| `304` | — | — | success | not emitted by v1 |
| `400` | `INVALID_REQUEST` | **no** | no | our request is wrong; e.g. a stale cursor |
| `401` / `403` | `UNAUTHENTICATED` / `FORBIDDEN` | **no** | no | one credential refresh, then fail |
| `404` | `NOT_FOUND` | **no** | **no** | a valid answer; on `latest-execution` it means "never executed" |
| `408` | `UPSTREAM_TIMEOUT` | yes | yes | |
| `409` / `422` | `UPSTREAM_INVALID_RESPONSE` | no | no | |
| `429` | `RATE_LIMITED` | yes, **only** if `Retry-After` fits the deadline | no | honouring `Retry-After` is mandatory; ignoring it amplifies the overload (REQ-RES-003) |
| `500` / `502` | `UPSTREAM_UNAVAILABLE` | **no** | yes | a deterministic server fault fails identically on retry |
| `503` / `504` | `UPSTREAM_UNAVAILABLE` / `UPSTREAM_TIMEOUT` | yes | yes | the canonical transient cases |
| connection refused, DNS failure, TLS handshake failure | `UPSTREAM_UNAVAILABLE` | yes | yes | REQ-EDGE-014 |
| mid-stream reset / truncated body | `UPSTREAM_UNAVAILABLE` | yes | yes | no partially decoded record may surface (REQ-EDGE-014) |
| non-JSON content type, malformed JSON | `UPSTREAM_INVALID_RESPONSE` | **no** | yes | REQ-EDGE-020 |
| empty body with `200` | `UPSTREAM_INVALID_RESPONSE` | no | yes | distinct from an empty collection (REQ-EDGE-019c) |
| major `schemaVersion` mismatch | `SCHEMA_VERSION_MISMATCH` | **no** | **abstains** | the breaker must not open on a healthy source it merely cannot parse — and must not *close* on one either, which is why this abstains rather than counting as a success; the call-time fallback handles it (REQ-EDGE-017) |
| client disconnect | — | **no** | **no** | must not trip the breaker (REQ-RES-011) |

`retry.max_attempts: 2` with full jitter (`rand(0, min(max_backoff,
base_backoff × 2^(n−1)))`). The lower attempt count than the ODS is deliberate:
at a 240 ms P95, a third attempt rarely fits inside a per-source budget, and
attempting it burns a scarce bulkhead permit for a guaranteed cancellation
(REQ-RES-002, REQ-RT-006).

---

## 10. Contract-test obligations

`test/contract/exsource_test.go`, run against `cmd/exsource` and, where
available, a staging EDS. Merge gate.

### 10.1 Structural

| Obligation | Assertion | Req |
|---|---|---|
| Endpoint set | exactly the four endpoints in §2 exist and respond | REQ-DS-004 |
| Mandatory fields | every record carries `executionId`, `resourceId`, `tenantId`, `status` | REQ-MAP-008 |
| Status vocabulary | every spelling in `spec/data-contracts.md` §6.2 is producible and maps correctly | REQ-MAP-004 |
| Ordering | list results are newest-first by `startedAt`; the BFF's re-sort is a no-op | REQ-API-012 |
| Pagination | `nextCursor` round-trips; the last page omits it; a cursor is never reused across resources | §5 |
| `limit` bounds | `limit=200` succeeds; the BFF rejects `limit=201` before the call | REQ-API-014 |
| Empty collection | a resource with no executions returns `200` with an empty `items` array and `total: 0`, not `404` | REQ-EDGE-019 |
| `latest-execution` on a never-executed resource | `404`, mapped to `LatestExecution = nil`, never a request-level `404` | §2.3 |
| Tenant scoping | a request with tenant B's `tenantId` never returns tenant A's records | REQ-SEC-006 |
| Timestamps | RFC 3339 with `Z`; `completedAt` null iff non-terminal | REQ-MAP-010 |
| Schema version | `schemaVersion` present in the body or the `X-Schema-Version` header | REQ-EDGE-017 |
| Body bound | no response exceeds 4 MiB at `limit=200` | REQ-DS-009 |
| Health | `/eds/v1/health` returns a member of the state enum | §2.4 |
| Cost premise | EDS P95 measured ≥ 3× ODS P95 over ≥ 500 samples — if this ever fails, §7's argument and the routing defaults need revisiting | REQ-PERF-001 |

### 10.2 Behavioural, driven by the stub's chaos knobs (`:9112`)

`cmd/exsource` serves `GET /healthz`, `GET /livez` and `GET /readyz` on its admin
port — the same three probes the BFF serves — so the compose stack and the
Kubernetes manifests can target `/readyz` on the stub as they do on the service.

`PUT /chaos` accepts any subset of the knobs below; `DELETE /chaos` resets them
to the defaults (120ms base latency, 60ms jitter); `GET /chaos` reports the
current values.

| Knob | Drives | Expected BFF behaviour | Req |
|---|---|---|---|
| `base_latency_ms` | baseline delay before every response. Defaults to `120ms`, which is the property the whole routing design works around — the EDS really is slower | optional branch ⇒ `206 partial` + `SOURCE_TIMEOUT` once past `per_source_timeout.execution`; required ⇒ `504` | REQ-EDGE-013 |
| `jitter_ms` | uniform jitter added to the baseline. Defaults to `60ms` | a realistic latency distribution rather than a constant | REQ-PERF-001 |
| `failure_rate` | fraction of calls answered `502` | retry only retryable statuses; breaker opens on sustained failure | REQ-RES-003, REQ-RES-005 |
| `unavailable` | every call answered `503`; `/eds/v1/health` reports `unavailable` | optional branch ⇒ `206 partial` + `SOURCE_UNAVAILABLE`; execution-only endpoints ⇒ `503` with no operational substitution | REQ-EDGE-004 |
| `malformed` | drops `executionId` from every record | `UPSTREAM_INVALID_RESPONSE`, not retried — the mapper rejects the record rather than passing it through | REQ-EDGE-020 |
| `schema_version` | overrides the declared contract version | an unaccepted version ⇒ `SCHEMA_VERSION_MISMATCH`; **the breaker is not tripped**; the call-time fallback serves where one is configured, else `502` | REQ-EDGE-017 |

One record-level control sits alongside them, and it is how a test puts the
system into the state where the precedence override fires:

| Endpoint | Effect |
|---|---|
| `POST /resources/{resourceId}/executions?operation=&state=&resultingState=&tenantId=` | Inserts a new execution at the head of that resource's list. Defaults to `state=IN_PROGRESS`, which the mapper turns into canonical `RUNNING`, so the `execution_overrides_when_running` rule applies to `status`/`subState` (REQ-PREC-003, REQ-EDGE-015) |

The seeded data is aligned with the operational stub's by the same index
arithmetic — `R00i` belongs to `acme` when `i mod 3 == 0`, `local` when
`i mod 3 == 1`, `globex` otherwise, and carries `(i mod 4) + 1` executions named
`E00i-0`.. — so the operational record's `in_flight_execution_ref` names an
execution this source really does report as running. That makes the precedence
rule observable in the compose stack with no manual setup.

### 10.3 Consumer-driven obligations on the EDS team

1. `executionId`, `resourceId`, `tenantId` and `status` are always present; their
   removal is a major version bump.
2. `tenantId` on a returned record is always the tenant that was requested.
3. Timestamps stay RFC 3339 with an explicit offset.
4. New `status` spellings are additive; existing spellings never change meaning.
5. `observedAt` reflects response generation, not record creation.
6. A resource with no executions returns `200` with an empty array — never `404`,
   and never `204`.
7. `latest-execution` returns `404` (not an empty `200`) when nothing has ever
   run, so the BFF can distinguish "never executed" from "list truncated".
