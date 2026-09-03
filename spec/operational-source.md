# Operational Data Source (ODS)

The low-latency, current-state source of truth. gRPC. Consumed by
`internal/datasource/operational` behind `OperationalPort` (REQ-DS-001).

Proto: `api/proto/operational/v1/operational.proto`.
Generated stubs: `internal/datasource/operational/opsv1`.
Reference stub binary: `cmd/opsource` (gRPC `:9101`, admin/chaos `:9111`).

---

## 1. Characteristics

| Property | Value | Consequence for the BFF |
|---|---|---|
| Protocol | gRPC / HTTP2, protobuf | multiplexed streams over one long-lived connection; head-of-line blocking is per-stream, not per-connection |
| Consistency | current state, eventually refreshed by a poller / event stream / reconciler | freshness is a first-class attribute, not an assumption (§4) |
| Latency profile | P50 ≈ 8 ms, P95 ≈ 45 ms, P99 ≈ 90 ms for `GetResource` | it is the *preferred* source for anything it can answer |
| Freshness lag | seconds to low minutes depending on `refresh_source` | TTL-based routing is meaningful; a 30 s TTL is a real discriminator |
| Data shape | resource-centric, wide records (config map, metric samples, topology) | full reads are the expensive operation; projection and probing matter |
| Vocabulary | `state`, `customer_ref`, `substate`, `PROVISIONING` | deliberately not canonical; mapped explicitly (REQ-MAP-003, REQ-MAP-005) |
| Tenancy | enforces isolation itself | BFF still enforces independently — defence in depth (REQ-SEC-004, REQ-SEC-006) |
| Idempotency | all five RPCs are reads | retry is always safe in principle, subject to classification (REQ-DS-008) |
| Cardinality | one record per (tenant, resourceId) | the probe memo key is `(tenant, resourceId)` |

**Why it is preferred.** Every field group except execution-derived ones is
suppliable only by the ODS (see the field catalog in `spec/routing-policy.md`
§1.2), and it is roughly an order of magnitude faster than the EDS. The routing
chain's rules 4, 6 and 8 all resolve to `OPERATIONAL` for that reason. The ODS is
the default answer; the EDS is what you fall back to when the ODS's answer is too
old or unavailable.

---

## 2. gRPC contract walkthrough

### 2.1 `GetResourceFreshness` — the probe

```proto
rpc GetResourceFreshness(GetResourceFreshnessRequest) returns (GetResourceFreshnessResponse);

message GetResourceFreshnessRequest  { RequestContext context = 1; string resource_id = 2; }
message GetResourceFreshnessResponse { string resource_id = 1; bool found = 2; FreshnessEnvelope freshness = 3; }
```

The reason this RPC exists is the reason the service exists (REQ-DS-002,
REQ-RT-003). The BFF needs exactly one fact to route — *how old is the record* —
and obtaining it by performing the full read would defeat the purpose.

| | `GetResource` | `GetResourceFreshness` |
|---|---|---|
| Response size | 2–40 KiB | < 200 B |
| Server work | index lookup + hydrate config/metrics/topology + serialize | index lookup on the freshness column |
| BFF timeout key | `sources.operational.call_timeout` | `sources.operational.freshness_probe_timeout` |
| Default timeout | 800 ms | 120 ms |
| Retry | per classification | **never** (REQ-RES-003) |
| Failure effect | request-affecting | verdict `UNKNOWN` → rule `ttl.unknown_freshness` |
| Target P95 | ≤ 45 ms | ≤ 15 ms (REQ-PERF-002) |

`found = false` is the not-found signal for the whole request and is negatively
cached for `cache.negative_ttl` (REQ-CACHE-005). An echoed `resource_id` that
disagrees with the request is `UPSTREAM_INVALID_RESPONSE`.

The adapter MUST NOT synthesise a freshness result from a full read; that is
asserted by `TestOperationalAdapter_FreshnessUsesProbeRPC` (REQ-DS-002).

### 2.2 `GetResource` — the full read

```proto
message GetResourceRequest { RequestContext context = 1; string resource_id = 2; repeated string field_mask = 3; }
```

`field_mask` is populated from the classifier's `RequiredFields`, translated
through the field catalog. `resource_configuration` sends
`["configuration", "freshness", "schema_version"]`; `resource_read` sends the
wider set; `resource_details` sends everything except `metrics` unless the
details view requires them. An empty mask means "all" and is used only when the
request type genuinely needs every group.

Projection is a *cost* optimisation on the source side, never a correctness
mechanism: a field absent because it was masked out is indistinguishable on the
wire from a field the source does not have, so the BFF only ever masks out groups
the request type does not require. This keeps REQ-EDGE-006 (partial payload)
decidable.

### 2.3 `GetResourceState` — the narrow read

```proto
message GetResourceStateResponse { string resource_id = 1; ResourceState state = 2; string substate = 3; FreshnessEnvelope freshness = 4; string in_flight_execution_ref = 5; }
```

Used exclusively by `resource_status`. It returns the `status` and
`subState` groups plus the freshness envelope, and nothing else. The
mapper produces a `Resource` with all other groups absent, and this MUST NOT
raise `PARTIAL_DATA` — the request type only requires what this RPC
returns (`spec/data-contracts.md` §5.1, REQ-EDGE-006).

Because it carries its own `FreshnessEnvelope`, a `resource_status` request whose
probe memo is warm can skip the probe entirely and still populate
`meta.freshness` accurately from the read's envelope.

### 2.4 `BatchGetResources`

```proto
message BatchGetResourcesRequest  { RequestContext context = 1; repeated string resource_ids = 2; }
message BatchGetResourcesResponse { repeated OperationalResource resources = 1; repeated string missing_resource_ids = 2; }
```

Connection-reuse / fan-in optimisation. Not used by any v1 endpoint (all six are
single-resource), but exposed on the port so that a future list endpoint does not
require an N+1 pattern (REQ-AGG-007). `missing_resource_ids` is consumed for
not-found accounting and is on the declared drop list for the response envelope
(`spec/data-contracts.md` §8).

Batch size is capped at 100 ids by the adapter; a larger request is split into
sequential batches with the same per-call timeout, never issued concurrently
(concurrency to the source is governed by the bulkhead, not by the caller).

### 2.5 `Health`

```proto
message HealthResponse { HealthState state = 1; string detail = 2; }
```

Polled by the background health poller at a jittered 5 s interval
(REQ-DS-006). It is **never** called on the request path — the router reads an
in-memory snapshot. This is what allows rules 2 and 7 to fire *before* dispatch,
so no doomed call is ever issued.

`HealthState` → routing availability:

| `HealthState` | Router treats as | Notes |
|---|---|---|
| `HEALTH_STATE_SERVING` | available | |
| `HEALTH_STATE_DEGRADED` | available | still dispatched; latency may be worse |
| `HEALTH_STATE_NOT_SERVING` | unavailable | rules 2/7 |
| `HEALTH_STATE_UNSPECIFIED` | unavailable | fail closed |
| *(poll failed / never succeeded)* | unavailable | |
| *(BFF breaker open)* | unavailable | breaker state ORs into the snapshot (REQ-RES-006) |

`detail` is logged and dropped from the response envelope.

### 2.6 `RequestContext`

```proto
message RequestContext { string tenant_id = 1; string correlation_id = 2; string principal = 3; }
```

Populated on **every** call by the adapter (REQ-DS-003):

- `tenant_id` — the authenticated tenant from the JWT claim, never from
  `X-Tenant-ID` alone (REQ-SEC-004).
- `correlation_id` — the inbound or generated id (REQ-API-010), enabling
  cross-protocol correlation with the EDS's `X-Correlation-ID` header.
- `principal` — the JWT subject, for the source's own audit trail.

W3C trace context travels separately via `otelgrpc` metadata propagation
(REQ-API-011); it is not duplicated into `RequestContext`.

---

## 3. Latency budget

`sources.operational` timeouts, and how they compose inside a request deadline
derived from `server.write_timeout` minus a response-build reserve.

| Phase | Config key | Default | Notes |
|---|---|---|---|
| Dial (first connection) | `dial_timeout` | 2s | off the request path — the pool is warmed at startup |
| Freshness probe | `freshness_probe_timeout` | 120ms | strictly `< call_timeout`, validated at startup (REQ-CFG-003) |
| Unary call | `call_timeout` | 800ms | narrowed to fit the remaining request deadline (REQ-RT-006) |
| Per-attempt (with retry) | derived | `call_timeout / max_attempts`, floor `min_viable_timeout` | so a retried call still fits its budget |
| Minimum viable | `resilience.min_viable_timeout` | 25ms | below this the source is dropped from the decision with a warning, not called (REQ-RT-006) |

Worked budget for `resource_details` (`BOTH`) with a 2 s request deadline:

```
2000ms request deadline
 −  60ms  response-build + encode reserve
 = 1940ms available for source work
    ├─ operational branch: min(call_timeout 800ms, 1940ms) = 800ms   [required]
    └─ execution branch:   min(call_timeout 1500ms, 1940ms) = 1500ms [optional]
    branches run concurrently ⇒ wall time = max(800, 1500) = 1500ms worst case
```

Because the branches are concurrent, the fan-out costs the slower source, not
their sum (REQ-PERF-006). Target latencies are REQ-PERF-001: `OPERATIONAL` P95
60 ms / P99 120 ms; `BOTH` P95 280 ms / P99 550 ms.

**Probe amortisation.** The probe memo (REQ-TTL-006) caps probe traffic at
`min(cache_ttl, 1s)` per `(tenant, resourceId)` per replica. For a hot resource
under `resource_status` (`cache_ttl: 3s`) that is at most 1 probe RPS per replica
per resource, independent of inbound rate.

---

## 4. Freshness envelope semantics

```proto
message FreshnessEnvelope {
  google.protobuf.Timestamp last_updated = 1;  // when the source last refreshed the record
  google.protobuf.Timestamp server_time  = 2;  // response-generation time, SOURCE clock
  string refresh_source = 3;                   // "poller" | "event-stream" | "reconciler"
  uint64 version = 4;                          // monotonic record version
}
```

### 4.1 `last_updated` and `server_time` — one clock, not two

Both timestamps come from the **source's** clock. That is the entire point: the
age can be computed as a single-domain subtraction in which any offset between
the BFF and the source cancels exactly.

```
age = server_time − last_updated          // preferred, exact, skew-immune
```

The BFF clock is used only to *observe* skew, never to compute age:

```
skewEstimate = bff_now − server_time      // diagnostic only
```

Handling (REQ-TTL-004, REQ-EDGE-010):

| Condition | Action |
|---|---|
| `server_time` present | use single-domain age; record `skewEstimate` |
| `|skewEstimate| > clock_skew_tolerance` (default 2s) | warn `CLOCK_SKEW_DETECTED`, keep the single-domain age |
| `|skewEstimate| > 10 × tolerance` | verdict `UNKNOWN` — the source clock is not trustworthy for any arithmetic |
| `server_time` absent | fall back to `bff_now − last_updated`, then subtract clamped tolerance so the result is biased **older**, never fresher |
| computed `age < 0` | clamp to `0`, warn `CLOCK_SKEW_DETECTED` |
| `last_updated` zero/absent | verdict `UNKNOWN` (never treated as the Unix epoch, REQ-MAP-010) |

The bias direction is deliberate: judging borderline data *older* costs an
unnecessary fallback or a stale-serve marker; judging it *fresher* serves wrong
data silently. A source running 10 s fast must never make 40 s-old data pass a
30 s TTL.

### 4.2 `refresh_source`

Diagnostic provenance of the refresh mechanism. It materially changes the
*expected* freshness distribution:

| `refresh_source` | Typical lag | Interpretation |
|---|---|---|
| `event-stream` | sub-second | data is near-live; a 30 s TTL will almost always be satisfied |
| `poller` | one poll interval | age is bounded but sawtoothed; TTL should exceed the poll interval |
| `reconciler` | minutes | age is long by design; TTL-based routing will frequently fall back |

It is logged at debug level and is on the
declared drop list for the response envelope (`spec/data-contracts.md` §8) — it
describes the source's internals, not the resource.

### 4.3 `version`

A monotonic per-record counter, mapped to `domain.Freshness.Version`. Its uses:

- **Conflict detection** between a cached entry and a fresh read: a cached entry
  with a *higher* version than a freshly read one indicates the source served a
  replica that has regressed; the fresher-versioned value wins and a
  `CONFLICT_RESOLVED` warning is emitted.
- **Probe/read coherence**: if the probe reported version `N` and the subsequent
  full read returns version `< N`, the read came from a lagging replica; the
  freshness meta reflects the read's (older) envelope, and the response is marked
  degraded.

`version = 0` means the source does not maintain versions; both mechanisms are
then disabled and freshness rests on timestamps alone.

### 4.4 What the envelope does *not* mean

- It is **not** a cache TTL. `cache_ttl` is a BFF-side concept and is never
  derived from the envelope (REQ-TTL-001).
- It is **not** a guarantee of correctness. `last_updated` says when the source
  last *refreshed*, not when the underlying reality last *changed*.
- It is **not** comparable to the EDS's `observedAt` for precedence purposes.
  The two describe different events on different clocks; comparing them to pick a
  winner is a category error (REQ-PREC-006, REQ-EDGE-009).

---

## 5. Health and degradation behaviour

### 5.1 Health snapshot construction

```
snapshot[OPERATIONAL] =
    breaker.State == OPEN                       → CIRCUIT_OPEN   (unavailable)
    bulkhead saturated for > acquire_timeout    → BULKHEAD_FULL  (unavailable for admission)
    last poll failed or older than 3 intervals  → UNKNOWN        (unavailable)
    poll returned NOT_SERVING                   → NOT_SERVING    (unavailable)
    poll returned DEGRADED                      → DEGRADED       (available)
    poll returned SERVING                       → SERVING        (available)
```

Read once per request and frozen for the whole rule chain (REQ-RT-010) so that no
two rules can disagree about whether the source is up.

### 5.2 Degradation ladder when the ODS is impaired

Applied by `internal/application` (REQ-RES-009, REQ-EDGE-003):

| Step | Condition | Result |
|---|---|---|
| 1 | `fallback != none` and the fallback can supply the required fields | route to EXECUTION, `degraded: true`, `execution_fallback_total++`; operational-only groups absent ⇒ `partial: true` |
| 2 | fresh cache entry exists | serve it, `cache.hit: true`, `degraded: false` |
| 3 | stale cache entry within `max_stale` and `allow_stale` | serve it, `degraded: true`, warning `STALE_DATA`, `stale_response_total++` |
| 4 | at least one field group obtainable | `206`, `partial: true` |
| 5 | otherwise | `503 NO_SOURCE_AVAILABLE` with `sources: [{OPERATIONAL, CIRCUIT_OPEN}]` |

For `resource_configuration` step 1 is structurally unreachable
(`fallback: none`, and the EDS cannot supply configuration), so the ladder is
2 → 3 → 5.

### 5.3 `DEGRADED` is dispatched, not skipped

A source reporting `HEALTH_STATE_DEGRADED` is still called. Skipping it would
convert a partial capacity problem into a total outage for every operational
field group. Degradation is handled by the per-source timeout and the breaker,
which are quantitative; health is qualitative and coarse.

---

## 6. Connection pooling and keepalive

Config subtree `sources.operational`:

```yaml
sources:
  operational:
    addr: ods.internal:9101
    dial_timeout: 2s
    call_timeout: 800ms
    freshness_probe_timeout: 120ms
    max_conns: 4
    keepalive:
      time: 30s                    # ping when idle this long
      timeout: 10s                 # ping ack deadline
      permit_without_stream: true  # keep NAT/LB state alive when idle
    tls:
      enabled: true
      ca_file: /etc/bff/tls/ods-ca.pem
      cert_file: /etc/bff/tls/client.pem
      key_file: /etc/bff/tls/client-key.pem
      server_name: ods.internal
      min_version: "1.3"
    retry:      { max_attempts: 3, base_backoff: 20ms, max_backoff: 200ms, jitter: full }
    breaker:    { failure_ratio: 0.5, min_requests: 20, window: 30s, cooldown: 10s, half_open_successes: 3 }
    bulkhead:   { max_concurrent: 64, acquire_timeout: 50ms }
```

### 6.1 Connection strategy (REQ-DS-005)

- **One `grpc.ClientConn`**, shared process-wide, created at startup and warmed
  before readiness flips to `200`. HTTP/2 multiplexes concurrent RPCs over it;
  per-request dialling would add a TLS handshake to every call.
- **`max_conns: 4`** sub-connections via round-robin over resolved backends, so
  a single backend's stream limit or a single TCP path's congestion does not cap
  throughput.
- **`WaitForReady(false)`** — mandatory. With `WaitForReady(true)` a call to an
  unavailable backend *queues* until the deadline and then surfaces as
  `DEADLINE_EXCEEDED`, which the breaker classifies as a timeout rather than an
  unavailability. Fail-fast keeps the classification honest and makes rule 7
  fire promptly.
- **Keepalive with `permit_without_stream: true`** — an idle connection through a
  load balancer with an idle timeout is silently reaped, and the next RPC pays a
  full reconnect inside its call budget. Pinging every 30 s keeps the path warm.
  `time` must be ≥ the server's `EnforcementPolicy.MinTime` or the server will
  send `GOAWAY` with `ENHANCE_YOUR_CALM`; 30 s is safely above the common 5 min
  default and above any server configured below it.
- **`MaxCallRecvMsgSize` = 4 MiB** (REQ-DS-009). A larger response is an error,
  not an OOM.
- **Interceptors**, outermost to innermost: `otelgrpc` (tracing/metrics) →
  correlation/tenant metadata injection → bulkhead → breaker → retry → timeout.
  Retry is *inside* the breaker so that a retried-and-failed call registers one
  logical failure, and *inside* the bulkhead so that retries do not acquire a
  second permit.

### 6.2 Startup warm-up

`cmd/bff` dials and issues one `Health` call before flipping `/readyz` to `200`
(REQ-API-003). This front-loads TLS handshake and HTTP/2 settings exchange so the
first real request does not pay them.

---

## 7. Retry and idempotency rules

All five RPCs are reads and are therefore idempotent (REQ-DS-008). Idempotency is
necessary but not sufficient — retry eligibility is decided by the typed error
classification, never by "an error occurred" (REQ-RES-003).

| gRPC code | `pkg/errs` code | Retryable | Counts toward breaker | Notes |
|---|---|---|---|---|
| `OK` | — | — | success | |
| `UNAVAILABLE` | `UPSTREAM_UNAVAILABLE` | **yes** | yes | the canonical transient case |
| `DEADLINE_EXCEEDED` | `UPSTREAM_TIMEOUT` | **yes**, if budget remains | yes | server-side or BFF-side deadline |
| `RESOURCE_EXHAUSTED` | `UPSTREAM_UNAVAILABLE` | yes, honouring backoff | yes | server is shedding; jitter matters most here |
| `ABORTED` | `UPSTREAM_UNAVAILABLE` | yes | yes | |
| `INTERNAL` | `UPSTREAM_UNAVAILABLE` | **no** | yes | a deterministic server bug will fail identically |
| `UNKNOWN` | `UPSTREAM_UNAVAILABLE` | no | yes | unclassifiable; do not amplify |
| `NOT_FOUND` | `NOT_FOUND` | **no** | **no** | a valid answer; negatively cached |
| `INVALID_ARGUMENT` | `INVALID_REQUEST` | **no** | no | our bug; retrying repeats it |
| `FAILED_PRECONDITION` | `UPSTREAM_INVALID_RESPONSE` | no | no | |
| `PERMISSION_DENIED` | `FORBIDDEN` | **no** | no | credential/scope problem |
| `UNAUTHENTICATED` | `UNAUTHENTICATED` | **no** | no | triggers one credential refresh, then fails |
| `UNIMPLEMENTED` | `SCHEMA_VERSION_MISMATCH` | **no** | yes | the source no longer offers this RPC — a major contract break |
| `CANCELED` (client) | — | **no** | **no** | client disconnect must not trip the breaker (REQ-RES-011) |
| decode/validation failure | `UPSTREAM_INVALID_RESPONSE` | **no** | yes | REQ-EDGE-020 |

### 7.1 Retry schedule

`retry.max_attempts: 3` counts **total attempts**, not additional ones. Delay
before attempt `n` is:

```
delay = rand(0, min(max_backoff, base_backoff × 2^(n−1)))     // full jitter
```

Full jitter (not equal jitter, not fixed backoff) because the alternatives
resynchronize clients after an outage: every client that failed at `t` retries at
`t + base`, producing a thundering herd exactly when the source is most fragile
(REQ-RES-002).

### 7.2 Retry budget

The cumulative retry schedule must fit inside the remaining request deadline. An
attempt that cannot complete before the deadline is **not made** — issuing it
burns a connection and a bulkhead permit to produce a guaranteed cancellation
(REQ-RES-002, REQ-RT-006).

### 7.3 The probe is never retried

A retry of `GetResourceFreshness` would cost more than the read it exists to
avoid. Probe failure yields verdict `UNKNOWN` and routes via rule 10
(REQ-TTL-005).

---

## 8. Contract-test obligations

`test/contract/opsource_test.go` runs the same suite against the reference stub
(`cmd/opsource`) and, in a pipeline with access, against a staging ODS. Passing
is a merge gate.

### 8.1 Structural

| Obligation | Assertion | Req |
|---|---|---|
| Service surface | all five RPCs exist and are callable | REQ-DS-003 |
| Probe is genuinely cheap | probe P95 ≤ 15 ms and probe P95 < 40% of `GetResource` P95 over ≥ 500 samples | REQ-PERF-002 |
| Probe returns no resource body | response contains only `resource_id`, `found`, `freshness` | REQ-DS-002 |
| `RequestContext` echoed/honoured | a call with a foreign `tenant_id` returns `NOT_FOUND` or `PERMISSION_DENIED`, never another tenant's record | REQ-SEC-006 |
| Enum totality | every `ResourceState` member is producible and maps per `spec/data-contracts.md` §6.1 | REQ-MAP-003 |
| Freshness envelope | `last_updated` and `server_time` are both populated on every read | REQ-TTL-004 |
| `server_time` is the source clock | two calls 1 s apart differ by ≈ 1 s in `server_time` regardless of BFF clock | REQ-EDGE-010 |
| `version` monotonicity | version never decreases for a given record across sequential reads | §4.3 |
| Schema version | `schema_version` is present and parses as semver | REQ-EDGE-017 |
| Response size bound | no response exceeds 4 MiB | REQ-DS-009 |
| Field mask honoured | a masked read omits unrequested groups and still returns `freshness` | §2.2 |
| Narrow read | `GetResourceState` returns `state`, `substate`, `freshness`, `in_flight_execution_ref` and no resource body | §2.3 |
| Health | `Health` returns a member of `HealthState` | §2.5 |

### 8.2 Behavioural, driven by the stub's chaos knobs (`:9111`)

`cmd/opsource` serves `GET /healthz`, `GET /livez` and `GET /readyz` on its admin
port — the same three probes the BFF serves — so the compose stack and the
Kubernetes manifests can target `/readyz` on the stub as they do on the service.

`PUT /chaos` accepts any subset of the knobs below, so a test can change one
dimension without restating the rest; `DELETE /chaos` resets them all; `GET
/chaos` reports the current values.

| Knob | Drives | Expected BFF behaviour | Req |
|---|---|---|---|
| `latency_min_ms` / `latency_max_ms` | slow responses, uniformly sampled between the two. The freshness probe is delayed by a **tenth** of the sampled value, so the probe stays materially cheaper than a read even under injected latency | per-source timeout, then ladder | REQ-EDGE-013 |
| `failure_rate` | fraction of calls answered `UNAVAILABLE` | retry only the retryable codes; breaker opens on sustained failure | REQ-RES-003, REQ-RES-005 |
| `unavailable` | every call fails; `Health` reports `NOT_SERVING` | rule 2 / rule 7 fire once the health snapshot updates; before that, `fallback.primary_failed` | REQ-EDGE-003 |
| `probe_unavailable` | fails **only** `GetResourceFreshness`, leaving ordinary reads working | verdict `UNKNOWN` → rule 10 applies `on_unknown_freshness` | REQ-TTL-005 |
| `stale_by_seconds` | adds to the age, pushing `last_updated` back | rule `ttl.operational.stale`; stale-serve within `max_stale` | REQ-EDGE-002 |
| `clock_skew_seconds` | shifts the reported `server_time` relative to real time | single-domain age unaffected; `CLOCK_SKEW_DETECTED` warning; gross offset ⇒ `UNKNOWN` | REQ-EDGE-010 |
| `partial` | drops the optional groups (ownership, metrics, topology, metadata, labels) | groups absent, not zero-filled; no false `partial` when not required | REQ-EDGE-006 |
| `schema_version` | overrides the declared contract version | an unaccepted version ⇒ `SCHEMA_VERSION_MISMATCH`; **the breaker is not tripped**; the call-time fallback serves where one is configured, else `502` | REQ-EDGE-017 |

Two record-level controls sit alongside the chaos knobs. They are *record state*,
not chaos state, so `DELETE /chaos` does not restore them:

| Endpoint | Effect |
|---|---|
| `POST /resources/{id}/age?seconds=N` | Adds N seconds to that record's age offset |
| `POST /resources/{id}/touch` | Zeroes the offset and bumps `freshness.version` |
| `POST /resources/{id}/in-flight?executionId=E` | Sets `in_flight_execution_ref`, the trigger for the precedence override on `status`/`subState` (REQ-PREC-003, REQ-EDGE-015) |
| `GET /resources` | Lists the seeded ids |

**Age is an offset, not a timestamp.** Each seeded record `R00i` carries a fixed
age offset of `(i mod 7) * 5` seconds, and `freshnessFor` recomputes
`last_updated = now − offset − stale_by` on every read. A real ODS is
continuously refreshed by a poller, so a record's age hovers around a steady
value; seeding absolute timestamps instead would mean every record drifts
monotonically into staleness the longer the stub runs, and a developer who left
the compose stack up over lunch would come back to a BFF routing everything to
the execution source for no visible reason. The offset keeps the demonstration
stable: `R007` is always fresh, `R013` is always 30s old, however long the stub
has been up.

Records are spread across tenants by the same arithmetic: `R00i` belongs to
`acme` when `i mod 3 == 0`, `local` when `i mod 3 == 1`, and `globex` otherwise,
so tenant isolation is exercised by the ordinary demo data. `R00i` is seeded with
an `in_flight_execution_ref` when `i mod 5 == 1`, naming an execution the
execution source really does report as running.

### 8.3 Consumer-driven obligations on the ODS team

The BFF depends on these properties. Changing any of them is a major version bump
(`spec/data-contracts.md` §9.3):

1. `GetResourceFreshness` remains materially cheaper than `GetResource`. If it
   ever costs the same, the routing design loses its justification.
2. `server_time` is the source's own clock at response generation, not a value
   copied from the request or from an NTP-corrected external source.
3. `last_updated` refers to the record's refresh, and never moves backwards for a
   given record.
4. `tenant_id` on a returned record is always the tenant that was requested.
5. Field numbers 1, 3, 5, 13 (`resource_id`, `tenant_id`, `state`, `freshness`)
   are load-bearing and may not be removed or retyped.
6. New `ResourceState` members are added, never redefined.
