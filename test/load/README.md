# Load tests

Six [k6](https://grafana.com/docs/k6/latest/) profiles. They measure latency and
throughput like any load test, and they also assert **routing policy**, which is
unusual and is only possible because every response publishes the decision that
produced it: `meta.routingDecision`, `meta.routingRule`, `meta.freshness`,
`meta.degraded`, `meta.partial` and `meta.cache`. A BFF that did not expose its
routing rule could not be load-tested for policy correctness at all.

| Profile | Shape | The question it answers |
|---|---|---|
| `smoke.js` | 1 VU, 1 iteration | Is the deployment wired up? Every endpoint, the admin surface, tenant isolation, and the rule id each endpoint fires. |
| `load.js` | Constant arrival rate, 200 rps, 10 min, weighted mix | Does it meet the SLOs, and is the TTL policy actually keeping traffic off the slow source? |
| `stress.js` | Staircase of arrival rates | Where is the knee, and does it shed load rather than queue it? |
| `spike.js` | 50 → 800 rps in 10s, then recover | Does it survive a burst, and does it *recover*? Does stampede protection hold? |
| `soak.js` | 150 rps for 4 h | Does anything drift — goroutines, heap, pools, routing ratios? |
| `degradation.js` | Load plus scheduled chaos | Does latency stay bounded when a source degrades, and does the degradation ladder behave? |

`common.js` holds the working set, the envelope accounting and the chaos client
that all six share.

## Running them

```bash
make compose-up            # the profiles assume the local stack

k6 run test/load/smoke.js
k6 run test/load/load.js
k6 run test/load/degradation.js

# Or through the Makefile, which starts the stack first:
make k6-load                                   # runs test/load/load.js
make k6-load K6_SCRIPT=test/load/smoke.js
make k6-load K6_BASE_URL=https://bff.dev.example.com K6_SCRIPT=test/load/smoke.js
```

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://localhost:8080` | The BFF's public API |
| `BFF_ADMIN_URL` | `http://localhost:9090` | Admin listener, used by `smoke.js` |
| `OPS_ADMIN_URL` | `http://localhost:9111` | Operational source chaos API |
| `EDS_ADMIN_URL` | `http://localhost:9112` | Execution source chaos API |
| `TENANT` | `local` | Sent as `X-Tenant-ID` |
| `BFF_TOKEN` | — | Sent as `Authorization: Bearer …` when the target enforces auth |
| `TOUCH_SETUP` | `true` | Reset the working set's ages in `setup()` — see below |
| `RATE`, `DURATION` | per profile | Offered load |
| `START_RATE`, `STEP`, `STAGES`, `STAGE_DURATION` | `stress.js` | Staircase shape |
| `BASELINE`, `PEAK` | `spike.js` | Spike shape |
| `PHASE_SECONDS` | `120` | `degradation.js` phase length |
| `COLD_SET` | `false` | `soak.js`: walk the working set sequentially instead of using the mix |

`smoke.js` and `load.js` run against any environment. `degradation.js` needs the
reference sources' chaos ports, so it runs against the compose stack or a
non-production environment — never against a real deployment.

### The working set, and why `setup()` touches the source

The operational source seeds `R001`..`R050`, assigns `R00i` to tenant `local`
when `i % 3 == 1`, and gives each record a fixed age of `(i % 7) * 5` seconds.
Two consequences shape these profiles:

- A profile that picked resource ids at random would spend a third of its
  requests collecting `404`s from tenant isolation. `LOCAL_RESOURCES` in
  `common.js` is the exact set that belongs to `local`.
- The seeded ages straddle the 10s `resource_status` TTL: roughly half the local
  set is fresh and half is not. Measuring against that mixture answers "what does
  a 50/50 hit rate cost?", which is a real question but not the baseline one. So
  `setup()` calls `POST /resources/{id}/touch` on each working-set record, which
  sets its age to zero, and the baseline profiles measure a system where the TTL
  policy is succeeding.

Set `TOUCH_SETUP=false` to measure the seeded spread instead. Expect
`ttl_hit_ratio` around 0.5 and a matching rise in `execution_fallback_ratio`; the
latency profile shifts toward the execution class.

`STATUS_RESOURCES` further excludes the resources where `i % 5 == 1`, because the
operational source seeds those with an in-flight execution reference and
`routing.defaults.resolve_in_flight_execution` therefore makes `/status` consult
the execution source for them. They answer `routingDecision: BOTH` — correctly —
which would blur the `operational_route_ratio` threshold into a blend of two
behaviours instead of a clean assertion about TTL routing.

## Reading the results

### The two latency measurements, and which to trust for what

| Metric | Measured by | Includes | Use it for |
|---|---|---|---|
| `http_req_duration` | k6, at the load generator | Network, connection reuse, TLS, k6's own client cost | The end-to-end experience; comparing environments |
| `bff_elapsed_ms` | the BFF, from `meta.elapsedMs` | Server-side wall time only | Comparing against REQ-PERF-001's budgets, and separating "the service got slower" from "the network got slower" |

Both are tagged by route class, so `http_req_duration{route:status}` and
`bff_elapsed_ms{route:status}` describe the same requests from the two ends.
When they diverge, the problem is between the generator and the service.

The SLOs (`spec/requirements.md` REQ-PERF-001, encoded in `LATENCY_SLO` in
`common.js`):

| Route class | p95 | p99 |
|---|---|---|
| `status`, `read`, `configuration` | 60 ms | 120 ms |
| `executions`, `execution_status` | 250 ms | 500 ms |
| `details` | 280 ms | 550 ms |
| cache hit (server-side) | 5 ms | 15 ms |

The cache-hit budget is asserted through `bff_elapsed_ms` rather than through
`http_req_duration`, because a 5 ms budget is smaller than the loopback and
client cost that `http_req_duration` unavoidably contains.

### The policy metrics

These are the ones that make a load run a statement about *behaviour*.

| Metric | Computed from | Reading it |
|---|---|---|
| `ttl_hit_ratio` | `meta.routingRule == "ttl.operational.fresh"` | How often the freshness TTL kept the request off the slow source. This is the design's headline number. |
| `operational_route_ratio` | `meta.routingDecision == "OPERATIONAL"` | Broader than the above: includes operational answers reached by other rules. |
| `execution_fallback_ratio` | rule is `ttl.operational.stale`, `health.primary_unavailable` or `fallback.primary_failed` | How much traffic the slow source is absorbing, and why. |
| `stale_serve_ratio` | rule is `degrade.stale_cache` | The bottom rung of the ladder. Above zero in a healthy run means something is wrong. |
| `degraded_ratio` | `meta.degraded` | Answers that are whole but older than intended, or came from a fallback. |
| `partial_ratio` | `meta.partial` | Answers missing a field group. Always paired with HTTP 206. |
| `cache_hit_ratio` | `meta.cache.hit` | Cache effectiveness for the working set. |
| `envelope_valid` | shape check on `meta` | A correctness assertion, not a performance one. |
| `shed_responses` | HTTP 429 + 503 | Deliberate refusal. Expected under saturation and during an outage. |
| `server_errors` | HTTP ≥ 500 excluding 503 | A defect at any load. Every profile thresholds this at zero. |
| `unconfigured_request_types` | rule is `guard.unconfigured_request_type` | A request type with no routing rule at all — a broken configuration, not a routing outcome. Thresholded at zero. |

**Shedding is not the same as breaking.** `429` (rate limited) and `503`
(no source available, or bulkhead saturated) are the service refusing work on
purpose. `500` means an internal error path was reached. Every profile allows the
first and forbids the second.

**And `206` is neither.** A partial answer is a complete, correct response with
some fields absent, counted by `partial_ratio` rather than by either counter. It
arises two ways: a source the request wanted did not answer, or the source that
did cannot hold every requested field. The second is the one to know about here —
when the operational source is down, `/resources/{id}` answered by the execution
source is a `206`, because the EDS supplies none of `configuration`, `owner`,
`metrics` or `topology`. `/status` in the same conditions stays `200`, because
`status` lists the EDS as a supplier. So a rise in `partial_ratio{route:read}`
during an operational outage is the design working, not a regression.

### Profile by profile

**`smoke.js`** — thresholds are absolute (`rate==1`, `rate==0`). It asserts the
rule id each endpoint fires: `/details` must fire `fields.span_both`,
`/executions` must fire `fields.execution_only`, `/status` on a touched record
must fire `ttl.operational.fresh`, and `/configuration` must fire a `ttl.*` rule
— explicitly **not** `fields.operational_only`.

It also asserts two things that are easy to regress silently: that
`/resources/{id}` is *complete* (`partial: false`) while the operational source is
healthy, and that `/executions/{id}` is **never** answered from cache. The second
follows from `execution_status` being configured `consistency: strong`: a strongly
consistent request bypasses the cache read entirely and is only ever written back,
so `meta.cache.hit` is always `false` there despite the configured
`cache_ttl: 2s`.

That last one is the assertion most likely to surprise. `/configuration`'s
required fields are exclusive to the operational source, so rule 4 does fire, but
it *pins* the source and clears the configured fallback and then falls through to
the TTL rules rather than terminating. Terminating there would skip the
`max_stale` ceiling for the one request type that has nowhere else to go. Rule 4
emits its own id only when the operational source is unavailable, and then with
`routingDecision: NONE` — which the smoke check would never see, because the
request would have failed first.

It also checks that `cache_ttl <= ttl` in the effective configuration reported by
`/config/routing`, and that another tenant's resource returns `404`.

**`load.js`** — a pass means the SLOs held *and* the policy worked. The
distinctive thresholds are `operational_route_ratio{route:status} > 0.95` and
`ttl_hit_ratio{route:status} > 0.90`. If latency passes but those fail, the
service is fast because it is doing something other than what the configuration
says; check `routing_decision_total` grouped by `routing_rule` on `:9090/metrics`.

**`stress.js`** — the knee is the first stage where `status` p99 crosses 120 ms
while the failure rate is still near zero. Past it, shedding should hold p99
roughly flat while `shed_responses` rises. A p99 that keeps climbing *without*
shed responses rising means work is queuing somewhere unbounded — check
`bulkhead_in_flight` and `bulkhead_rejected_total`. A falling
`operational_route_ratio` means the test tripped a source breaker, so everything
past that stage describes a degraded system rather than a saturated one.

**`spike.js`** — `p90` near the baseline budget with a much larger `p99` is the
expected shape: the spike is the tail. `p90` also elevated means the service did
not recover during the three-minute recovery window, and that window rather than
the spike is what you are looking at. Cross-check
`operational_source_latency` across the spike: if it rose in proportion to the
request rate, the burst reached the source amplified and stampede collapsing
(singleflight plus the Redis lock) did not work.

**`soak.js`** — k6 thresholds are whole-run aggregates and cannot by themselves
prove the absence of drift. Compare the summary against a 20-minute `load.js` run
at the same rate: matching percentiles mean no drift. Then look at the
process-level series over the run window — `go_goroutines`,
`go_memstats_heap_inuse_bytes`, `bulkhead_in_flight` — which are what actually
grow when something is leaking. `COLD_SET=true` walks the working set
sequentially, which stops the cache from hiding a per-key allocation problem.

**`degradation.js`** — prints one row per chaos phase. The rows are the result:

| Phase | What is broken | What the row should show |
|---|---|---|
| `baseline` | nothing | The control. Every other row is read against this one. |
| `eds_slow` | execution source at 2500 ms, past its 1500 ms per-source budget | `status` p95/p99 identical to baseline — a fresh operational record means the execution source is not on that path at all. `details` p99 near 1500–2000 ms, **not** near 2500 ms, because the per-source timeout is what bounds it. `partial%` near 1.0, which is the proof that the bound was achieved by degrading rather than by failing. |
| `eds_down` | execution source refusing | `status` unchanged; `details` back to baseline latency and `partial%` near 1.0. |
| `ods_stale` | operational records aged 60 s | `ops%` collapses, `fallback%` rises, and `status` latency moves to the execution class. This is the design working, not failing. `/status` stays `200` — `status` is a field the execution source supplies — whereas a `read` in the same conditions is a `206`. |
| `both_down` | total outage | `stale%` is how much traffic the cache absorbed; the rest is `503 NO_SOURCE_AVAILABLE`, which is a correct answer. Latency stays bounded. |
| `recover` | nothing | Every column returns to the baseline row. If `ops%` stays low, a breaker is still open: it needs `open_timeout` (5 s) plus two successful probes to close. |

The invariants across every phase are `server_errors == 0` and
`envelope_valid > 0.98` — degradation must never be an internal error, and the
degraded and partial envelopes are the ones most likely to be built wrong.

### Cross-checking against the service's own metrics

The k6 numbers and `:9090/metrics` should tell the same story. During or right
after a run:

```bash
curl -s localhost:9090/metrics | grep -E \
  'routing_decision_total|operational_ttl_(hit|miss)_total|execution_fallback_total'
curl -s localhost:9090/metrics | grep -E \
  'partial_response_total|stale_response_total|circuit_breaker_state'
curl -s localhost:9090/metrics | grep -E \
  'bulkhead_in_flight|bulkhead_rejected_total|cache_(hit|miss|error)'
```

`ttl_hit_ratio` from k6 should track `operational_ttl_hit_total` divided by
`operational_ttl_hit_total + operational_ttl_miss_total`. A disagreement means
requests were served from cache without re-entering the router, which is expected
and is why `cache_hit_ratio` needs to be read alongside it.

### Restoring the environment

Every profile calls `chaos.reset()` in `setup()` and `teardown()`, so a failed or
interrupted run should not leave the sources broken. If one is interrupted hard:

```bash
curl -sX DELETE http://localhost:9111/chaos
curl -sX DELETE http://localhost:9112/chaos
```

Ages set by `touch` are not restored by a chaos reset — they are record state,
not chaos state. Recreate the seeded spread by restarting `opsource`:

```bash
docker compose restart opsource
```

### Machine-readable output

```bash
k6 run --out json=results.json test/load/load.js
k6 run --summary-export=summary.json test/load/load.js
```

`spec/testing.md` §6.3 describes gating a release on these results. There is no
Go harness in this directory that parses the summary today — k6's own exit code
is the gate, which is non-zero on any threshold breach, so
`k6 run --summary-export=summary.json test/load/load.js` in CI already fails the
build on a breach. `summary.json` is kept for trend comparison between runs
rather than for a second round of assertions.
