# TTL-Aware BFF — Architecture

This document is the visual companion to the specifications. It shows the same
system twelve ways: five levels of structure, five request paths, one state
machine and one failure-mode map. The prose exists to say what each picture is
claiming and why the design is that shape; every claim in it is checkable
against `internal/**`, `configs/bff.yaml` and `docs/DESIGN-CONTRACT.md`.

Each diagram is also a standalone file under [`diagrams/`](diagrams/) so it can be
rendered on its own.

**Related documents.** [`DESIGN-CONTRACT.md`](DESIGN-CONTRACT.md) freezes names,
ports, keys and rule ids. [`../spec/requirements.md`](../spec/requirements.md) is
the requirement register. [`../spec/routing-policy.md`](../spec/routing-policy.md)
is the definitive rule-by-rule policy text. [`runbook.md`](runbook.md) is what you
read at 3am.

---

## 0. The problem, and the shape of the answer

A console needs one API over two backends with incompatible performance
profiles. The **Operational Data Source** (ODS, gRPC) holds current resource
state and answers in milliseconds. The **Execution Data Source** (EDS, REST)
holds workflow, history and audit records, structurally slower, and holds data
the ODS simply does not have. Naively fanning out to both on every request makes
every response as slow as the slower source; naively reading only the fast source
makes some answers wrong.

The answer is to make *how old the operational copy is* an explicit, configured,
per-request-type input to routing. If the operational record is inside its
freshness TTL, the request is answered from the fast source and the slow source
is never contacted. If it is not, a named rule decides what happens instead. The
decision is always attributable: every response carries the id of the rule that
produced it.

Three ideas carry the whole design:

1. **Two different TTLs, never conflated.** `ttl` is how old the *source's*
   observation may be. `cache_ttl` is how long the BFF may reuse *its own*
   computed answer. Configuration validation refuses a `cache_ttl` larger than
   the `ttl`, because that would let the cache serve data the freshness policy
   already calls stale.
2. **A rule chain, not nested conditionals.** Eleven ordered predicates, first
   match wins, each with a frozen id that shows up in metrics, traces and the
   response envelope.
3. **A degradation ladder with named rungs.** Preferred source → configured
   fallback at routing time → configured fallback at call time → partial response
   → stale cache serve → 503. Each rung is a policy decision with its own
   observable signal.

---

## 1. Context (C4 level 1)

```mermaid
C4Context
    title System context — TTL-Aware BFF

    Person(operator, "Operations console user", "Watches resource state and workflow progress. Polls /status; opens /details on demand.")
    Person_Ext(sre, "SRE / on-call", "Reads dashboards and the runbook; drives cache invalidation and config reloads.")

    System_Boundary(bff_boundary, "Backend for Frontend") {
        System(bff, "ttl-aware-bff", "Go service. Classifies a request, evaluates operational freshness against a per-request-type TTL, routes to one source or both, merges by precedence, and degrades on a fixed ladder. HTTP :8080, admin :9090.")
    }

    System_Ext(ods, "Operational Data Source (ODS)", "gRPC :9101. Current resource state. Cheap GetResourceFreshness probe plus GetResource / GetResourceState reads.")
    System_Ext(eds, "Execution Data Source (EDS)", "REST :9102. Workflow executions, history and audit. Structurally slower; holds data the ODS cannot supply.")
    System_Ext(cache, "Response cache", "In-process L1 and/or Redis L2. Holds the BFF's own computed answers, not source records.")
    System_Ext(idp, "Identity provider", "Issues the JWT carrying sub, tenant_id and roles. JWKS or HS256.")
    System_Ext(otel, "Telemetry backend", "OTLP collector, Prometheus scrape of :9090/metrics, trace storage.")

    Rel(operator, bff, "GET /api/v1/resources/...", "HTTPS + JWT")
    Rel(sre, bff, "GET :9090 /metrics, /config/routing, /readyz", "HTTP, cluster-internal")
    Rel(bff, ods, "GetResourceFreshness, GetResourceState, GetResource", "gRPC")
    Rel(bff, eds, "GET /eds/v1/executions, /latest-execution", "HTTPS/JSON")
    Rel(bff, cache, "cache-aside GET/SET, stale read", "Redis protocol / in-process")
    Rel(bff, idp, "Fetch JWKS", "HTTPS")
    Rel(bff, otel, "Export spans and metrics", "OTLP/gRPC")

    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

**What it is showing.** The BFF owns no data. It owns a *decision* — which source
answers, and whether the answer it already has is good enough. That is the entire
value it adds, and it is why the response envelope is as elaborate as it is: a
service whose product is a decision has to publish the decision.

**Why this shape.** Two boundaries matter here. The first is that the console
never talks to the ODS or the EDS directly; if it did, every client would have to
reimplement freshness policy, and the two clients would eventually disagree. The
second is that the SRE surface (`:9090`) is a different listener from the product
surface (`:8080`), unauthenticated and never routed through the ingress, so
`/metrics` and `/config/routing` cannot be reached from the internet and cannot be
starved by API rate limiting.

---

## 2. Containers (C4 level 2)

```mermaid
C4Container
    title Container view — the deployable units and what they speak

    Person(operator, "Operations console user")

    System_Boundary(sys, "ttl-aware-bff deployment") {
        Container(api, "BFF public API", "Go, net/http :8080", "Six GET routes under /api/v1. Auth, rate limit, body limit, request timeout, correlation, access log.")
        Container(admin, "BFF admin listener", "Go, net/http :9090", "/livez /readyz /healthz /metrics /buildinfo /config/routing. Unauthenticated, never exposed through the ingress.")
        ContainerDb(l1, "L1 cache", "in-process map, max_entries 20000, ttl 2s", "Per-replica. Holds cache.Entry values.")
        ContainerDb(l2, "L2 cache", "Redis 7", "Shared across replicas. Physical lifetime = cache_ttl + stale_grace (5m).")
    }

    System_Boundary(sources, "Reference data sources (compose stack)") {
        Container(ods, "opsource", "Go, gRPC :9101, admin :9111", "Operational Data Source stub. 50 seeded resources R001..R050, each with a fixed AGE OFFSET of (i mod 7) x 5s rather than a seeded timestamp, so the demonstration does not drift. Admin serves /healthz /livez /readyz. Chaos knobs: latency, failure_rate, unavailable, probe_unavailable, stale_by, clock_skew, partial, schema_version.")
        Container(eds, "exsource", "Go, REST :9102, admin :9112", "Execution Data Source stub. 120ms base latency by default. Admin serves /healthz /livez /readyz. Chaos knobs: base_latency, jitter, failure_rate, unavailable, malformed, schema_version.")
    }

    System_Boundary(obs, "Telemetry") {
        Container(collector, "otel-collector", "otel/opentelemetry-collector-contrib", "OTLP :4317 in; Prometheus exporter :8889 out.")
        ContainerDb(jaeger, "jaeger", "all-in-one, UI :16686", "Trace storage.")
        ContainerDb(prom, "prometheus", "UI :9091 on the host", "Scrapes the collector and the BFF admin port.")
        Container(grafana, "grafana", "UI :3000", "Dashboards over Prometheus and Jaeger.")
    }

    Rel(operator, api, "GET /api/v1/resources/{id}/...", "HTTPS + JWT, X-Tenant-ID")
    Rel(api, l1, "Get / Set / GetStale")
    Rel(api, l2, "Get / Set / GetStale, stampede lock")
    Rel(api, ods, "freshness probe, then narrow or full read", "gRPC, call_timeout 400ms, probe 120ms")
    Rel(api, eds, "latest-execution, executions", "HTTP, call_timeout 2s")
    Rel(api, collector, "spans + metrics", "OTLP/gRPC")
    Rel(admin, prom, "scraped on /metrics", "HTTP")
    Rel(collector, jaeger, "spans", "OTLP")
    Rel(prom, collector, "scrape :8889", "HTTP")
    Rel(grafana, prom, "PromQL", "HTTP")
    Rel(grafana, jaeger, "trace lookup", "HTTP")

    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="2")
```

**What it is showing.** Ports are contract, not convention: they are fixed in
`docs/DESIGN-CONTRACT.md` §1 and every manifest, compose service and test harness
reads them from there. `opsource` and `exsource` are reference implementations of
the two sources, complete with the chaos controls that let the failure tests
drive real degradation instead of mocking it.

**Why two cache tiers.** L1 is a per-replica map with a 2s TTL; L2 is Redis,
shared. The split is a latency/coherence trade: L1 removes the network hop for
the hottest keys, and its very short TTL bounds how far two replicas can disagree.
A promotion from L2 into L1 carries the *remaining* lifetime of the L2 entry, not
a fresh L1 TTL, so promotion can never extend an entry's life.

**Why the timeouts are asymmetric.** `sources.operational.call_timeout` is 400ms
and `sources.execution.call_timeout` is 2s. The ODS is supposed to be fast, so a
slow answer from it is a signal rather than something to wait out. The EDS
genuinely is slower; what stops that from mattering is the bulkhead —
`max_concurrent: 64` for the EDS versus `256` for the ODS — which caps how much
of the BFF a slow execution source can occupy.

---

## 3. Components: the package graph

```mermaid
flowchart TB
    subgraph transport["transport — knows HTTP, knows nothing about sources"]
        api["api<br/>server, routes, admin"]
        handler["api/handler"]
        mw["api/middleware"]
        resp["api/response"]
    end

    subgraph orchestration["orchestration — the only place the pieces meet"]
        app["application<br/>application.Service"]
    end

    subgraph policyLayer["policy — pure decisions, no I/O"]
        cls["classifier"]
        rt["router"]
        pol["policy<br/>precedence + field catalog"]
        fr["freshness"]
    end

    subgraph plumbing["plumbing"]
        agg["aggregation"]
        ca["cache"]
        ds["datasource<br/>ports + adapters"]
        map["mapper"]
        res["resilience"]
        sec["security"]
    end

    subgraph foundation["foundation — depended on, depends on nothing internal"]
        dom["domain"]
        cfg["config"]
        obs["observability"]
        errs["pkg/errs"]
        corr["pkg/correlation"]
        opsv1["datasource/operational/opsv1<br/>generated gRPC stubs"]
    end

    api --> handler
    api --> mw
    api --> resp
    api --> app
    api --> cls
    api --> sec
    api --> res
    api --> obs
    api --> cfg
    api --> dom
    api --> errs
    api --> corr

    app --> rt
    app --> cls
    app --> agg
    app --> ca
    app --> ds
    app --> fr
    app --> pol
    app --> obs
    app --> cfg
    app --> dom
    app --> errs
    app --> corr

    rt --> cls
    rt --> fr
    rt --> pol
    rt --> cfg
    rt --> dom

    cls --> pol
    cls --> cfg
    cls --> errs

    pol --> cfg
    pol --> dom

    fr --> ds
    fr --> dom

    agg --> ds
    agg --> pol
    agg --> dom
    agg --> errs

    ca --> cfg
    ca --> dom

    ds --> map
    ds --> res
    ds --> opsv1
    ds --> cfg
    ds --> dom
    ds --> errs
    ds --> corr

    map --> opsv1
    map --> dom
    map --> errs

    res --> cfg
    res --> errs

    sec --> cfg
    sec --> errs

    obs --> cfg
    obs --> corr

    classDef found fill:#eef2f7,stroke:#5b6b7f,color:#1b2733
    classDef pure fill:#eef7ee,stroke:#4b7a4b,color:#1b2733
    class dom,cfg,obs,errs,corr,opsv1 found
    class cls,rt,pol,fr pure
```

**What it is showing.** Every arrow is a real import edge, read from the source.
Four properties are load-bearing and each is visible as an *absence* in the graph:

| Property | Visible as |
|---|---|
| `domain` is the canonical model and has no internal dependencies | no arrow leaves `domain` |
| No package below the transport layer knows about HTTP | nothing points at `api`, `api/handler`, `api/response`, `api/middleware` |
| Routing is a pure function | `router` imports only `classifier`, `config`, `domain`, `freshness`, `policy` — no adapters, no cache, no observability |
| The pieces meet in exactly one place | `application` is the only package importing `router` **and** `cache` **and** `datasource` **and** `aggregation` |

Two consequences follow. The router's tests are truth tables over a value struct,
with no fakes, no HTTP and no clock. And the request lifecycle is testable without
a server, because `application` speaks only ports and canonical types.

`observability` is imported by `application` and `api` but by none of the pure
policy packages. Those packages report events through small `Hooks` structs
(`router.Hooks`, `cache.Hooks`, `aggregation.Hooks`) that the composition root
wires to metric instruments. This keeps `router` free of a telemetry dependency
without giving up per-rule metrics.

---

## 4. Request path: a TTL hit

```mermaid
sequenceDiagram
    autonumber
    actor UI as Console
    participant H as api/handler
    participant CL as classifier
    participant CA as cache.Manager
    participant SVC as application.Service
    participant R as router
    participant F as freshness.Manager
    participant ODS as Operational Data Source
    participant EDS as Execution Data Source

    UI->>H: GET /resources/R007/status<br/>X-Tenant-ID: local
    H->>CL: Classify(route, tenant, resource)
    CL-->>H: type=resource_status<br/>fields={status}<br/>consistency=bounded
    H->>SVC: GetResourceStatus(classification)
    SVC->>CA: GetOrLoad(key, cache_ttl=3s, loader)
    CA-->>SVC: miss

    rect rgb(238, 247, 238)
        note over SVC,R: loader — the cache-miss path
        SVC->>R: Select(classification, health snapshot)
        R->>F: Assess(tenant, R007, ttl=10s, skew=2s)
        F->>ODS: GetResourceFreshness (probe, 120ms budget)
        ODS-->>F: last_updated, server_time, version
        F-->>R: FRESH, age 0.4s <= ttl 10s
        R-->>SVC: Target=OPERATIONAL<br/>Rule=ttl.operational.fresh
        SVC->>ODS: GetResourceState (narrow read, 400ms budget)
        ODS-->>SVC: state, substate, freshness envelope,<br/>in_flight_execution_ref = ""
        note over SVC,EDS: no in-flight reference, so resolve_in_flight_execution<br/>does not fire and the EDS is not called at all
        SVC->>SVC: merge (single candidate) + project to statusView
    end

    SVC->>CA: store entry (logical 3s, physical 3s + stale_grace 5m)
    SVC-->>H: Envelope{data, meta}
    H-->>UI: 200 OK<br/>X-BFF-Freshness: FRESH, X-BFF-Source: OPERATIONAL<br/>meta.routingRule=ttl.operational.fresh<br/>meta.sources=[OPERATIONAL], degraded=false
```

**What it is showing.** The path the whole system exists to optimise: one cheap
probe plus one narrow gRPC read, and the expensive source is never touched.

**Three details worth naming.**

The probe is a separate RPC with its own budget (`freshness_probe_timeout: 120ms`
against a `call_timeout: 400ms`). Configuration validation enforces
probe < call. If the probe were not materially cheaper than the read it avoids,
the design would be worthless — a miss would pay probe + read.

The probe result is memoised for `probe_cache_ttl: 1s`, and what is memoised is
the *observation* (`last_updated`, `server_time`, `version`), never the verdict. A
cached verdict stops ageing: an entry judged FRESH at age 9.8s against a 10s TTL
would still claim FRESH five seconds later. Storing the observation and
recomputing keeps the memo honest.

Recomputing alone is not quite enough, though, and `cachedProbe.age` is the piece
that closes the gap. `last_updated` is a fact about the source and does not move,
but `server_time` is the instant the source *answered* — so replaying the pair
verbatim would recompute the age the record had when the probe was taken, not the
age it has now. The manager therefore advances the memoised `SourceTime` by the
time the entry has spent in the memo. That keeps the subtraction inside the
source's own clock domain, which is what makes it skew-proof, while still
accounting for the wall time that has passed, so **a memoised probe can never
make a record look fresher than it is.**

The `/status` endpoint uses `GetResourceState`, a narrow RPC, rather than the full
`GetResource`. The tightest endpoint should not pay for configuration, metrics and
topology it is about to discard. Everything else still goes through the same merge
path, which is why `/status` and `/details` cannot disagree about a resource's
status.

---

## 5. Request path: a TTL miss

```mermaid
sequenceDiagram
    autonumber
    actor UI as Console
    participant SVC as application.Service
    participant CA as cache.Manager
    participant R as router
    participant F as freshness.Manager
    participant ODS as Operational Data Source
    participant EDS as Execution Data Source

    UI->>SVC: GET /resources/R013/status (tenant local)
    SVC->>CA: GetOrLoad(key, cache_ttl=3s)
    CA-->>SVC: miss
    SVC->>R: Select(...)
    R->>F: Assess(tenant, R013, ttl=10s)
    F->>ODS: GetResourceFreshness
    ODS-->>F: last_updated = now-30s
    F-->>R: STALE, age 30s > ttl 10s

    rect rgb(253, 246, 236)
        note over R: rule ttl.operational.stale, branch (a)<br/>fallback=execution and EDS is available,<br/>so a current answer from the slow source beats<br/>a stale answer from the fast one
        R->>R: hooks.OnTTL(hit=false) -> operational_ttl_miss_total++<br/>hooks.OnFallback(OPERATIONAL -> EXECUTION) -> execution_fallback_total++
    end

    R-->>SVC: Target=EXECUTION<br/>Rule=ttl.operational.stale<br/>Primary=EXECUTION
    SVC->>EDS: GET /eds/v1/resources/R013/latest-execution
    EDS-->>SVC: execution record (~120ms base latency)
    SVC->>SVC: merge: operational candidate absent,<br/>status comes from the execution record
    SVC-->>UI: 200 OK<br/>meta.routingDecision=EXECUTION<br/>meta.routingRule=ttl.operational.stale<br/>meta.freshness.state=UNKNOWN (execution data carries no freshness TTL)
```

**What it is showing.** A TTL miss is not an error and not a degradation by
itself; it is a *route change*, and it is the point at which the request starts
paying the EDS latency profile.

**Why the branch order is fallback-first.** Rule 9 tries the configured fallback
before it considers serving stale operational data. A current answer from the slow
source beats a stale answer from the fast one — but only when the slow source can
actually supply the fields. `resource_configuration` has `fallback: none` precisely
because the EDS structurally cannot supply configuration; a fallback there would be
a lie rather than a degradation. When no fallback applies, rule 9 falls to serving
stale operational data if `allow_stale` permits and the age is within `max_stale`,
and refuses outright otherwise.

**Why the reported freshness is UNKNOWN.** Only the operational source publishes a
freshness envelope. An execution-sourced answer reports `UNKNOWN` rather than
borrowing the operational verdict, because the EDS makes no freshness guarantee and
claiming one would be a fabrication.

---

## 6. Request path: the BOTH fan-out

```mermaid
sequenceDiagram
    autonumber
    participant SVC as application.Service
    participant AGG as aggregation.FanOut
    participant T1 as task ods.read<br/>required=true, 400ms
    participant T2 as task eds.latest<br/>required=false, 1500ms
    participant T3 as task eds.history<br/>required=false, 1500ms
    participant ODS as Operational Data Source
    participant EDS as Execution Data Source
    participant P as policy.SourcePrecedence

    SVC->>SVC: route -> Target=BOTH, Rule=fields.span_both<br/>(required fields include configuration, operational-only,<br/>and latestExecution, execution-only)<br/>required_sources {operational:true, execution:false}
    SVC->>AGG: FanOut([ods.read, eds.latest, eds.history])

    par operational branch
        AGG->>T1: run with ctx deadline 400ms
        T1->>ODS: GetResource
        ODS-->>T1: full record (~5ms)
        T1-->>AGG: ok
    and execution latest
        AGG->>T2: run with ctx deadline 1500ms
        T2->>EDS: GET /resources/R007/latest-execution
        EDS-->>T2: execution (~120-180ms)
        T2-->>AGG: ok
    and execution history
        AGG->>T3: run with ctx deadline 1500ms
        T3->>EDS: GET /executions?resourceId=R007&limit=25
        EDS-->>T3: page (~120-180ms)
        T3-->>AGG: ok
    end

    AGG-->>SVC: FanOutResult{Partial:false,<br/>Elapsed = max(branch), not the sum}

    note over SVC: elapsed is the slowest branch. Two EDS calls in parallel<br/>cost one EDS round trip, which is REQ-PERF-006.

    SVC->>SVC: EvaluateFreshness(operational envelope, ttl=30s)
    SVC->>P: Resolve("status", [operational candidate, execution candidate])
    alt latest execution is in progress and field is in execution_overrides_when_running
        P-->>SVC: winner=EXECUTION, rule=precedence.execution_overrides_running
    else otherwise
        P-->>SVC: winner=OPERATIONAL, rule=precedence.configured_order
    end
    SVC-->>SVC: meta.provenance records the winner per field

    note over AGG,SVC: If eds.latest or eds.history had failed, they are optional:<br/>FanOutResult.Partial=true, one warning each<br/>(SOURCE_TIMEOUT or SOURCE_UNAVAILABLE), response 206.<br/>If ods.read had failed, it is required: fan-out returns an error<br/>and the call-time fallback / stale ladder takes over.
```

**What it is showing.** Three concurrent calls, each with its own derived context
and its own budget from
`routing.request_types.resource_details.per_source_timeout` (`operational: 400ms`,
`execution: 1500ms`).

**Why the fan-out does not cancel on first error.** It deliberately does not use
errgroup's cancel-on-first-error behaviour. If the execution branch fails at 50ms,
cancelling the still-running operational call would throw away data the response
could have used and would turn a partial answer into no answer at all. Every task
runs to completion or exhausts its own budget, and one source's timeout cannot
cancel another's call.

**Required versus optional is a configuration decision.**
`required_sources: {operational: true, execution: false}` is why a slow or failing
EDS yields a `206` with `partial: true` and a warning naming the source, rather
than a `500`. The heavy endpoint degrades to a fast operational-only answer instead
of inheriting the EDS latency profile as a hard dependency. `eds.history` is marked
optional in code regardless of configuration: execution history is never worth
failing a details view for.

**Merging is not last-writer-wins.** Every field both sources can supply goes
through `SourcePrecedencePolicy`, and the winner is recorded in
`meta.provenance`. The resolution order deliberately excludes "whichever timestamp
is newer": the EDS can legitimately hold a newer timestamp for a status it is only
*predicting* from a workflow, while the ODS holds the older but observed truth.
Choosing by recency would silently invert the intended precedence. The only way
execution data outranks operational data is the explicit
`execution_overrides_when_running: [status, subState]` list, and only while a
workflow is actually in progress.

**The `resolve_in_flight_execution` bridge.** `/status` is an operational-only
read, so the precedence override above could never fire for it — the execution
candidate would not exist. Without something extra, `/status` and `/details` would
report different statuses for the same resource mid-workflow, and the UI would see
the answer change depending on which screen it was on. So when the operational
record itself declares an `in_flight_execution_ref`,
`routing.defaults.resolve_in_flight_execution: true` makes the BFF fetch that one
execution with its own `in_flight_lookup_timeout: 300ms`. The common case pays
nothing, because the extra call is made only when the record says a workflow is
running. It is best-effort: a failure is logged at debug and the operational answer
stands unchanged.

**The marker alone never confers authority.** `Merger.Merge` sets
`ExecutionInProgress` only when there is an execution *candidate* whose status is
actually `InProgress`; an `in_flight_execution_ref` with no such candidate is
carried into `meta` and into `data.inFlightExecutionId` for information, and the
id is recorded on the precedence context, but the override does not fire. A
marker left behind by a workflow that has since finished would otherwise let a
completed execution's *predicted* status outrank the operational source's
*observed* one — and `/details` would then disagree with `/status`, which is
exactly what this bridge exists to prevent.

---

## 7. Request path: the call-time fallback

```mermaid
sequenceDiagram
    autonumber
    actor UI as Console
    participant SVC as application.Service
    participant R as router
    participant BRK as resilience.Breaker (operational)
    participant ODS as Operational Data Source
    participant EDS as Execution Data Source

    UI->>SVC: GET /resources/R007 (resource_read, fallback: execution)
    SVC->>R: Select(...) with health snapshot
    BRK-->>R: state=closed -> Health{Available:true, Detail:"HEALTHY"}
    R-->>SVC: Target=OPERATIONAL, Rule=ttl.operational.fresh<br/>Fallback=EXECUTION carried on the decision

    SVC->>ODS: GetResource (400ms budget, bounded retry)
    ODS--x SVC: UNAVAILABLE (x3 attempts, retry budget exhausted)
    ODS-->>BRK: failures recorded in the rolling window

    rect rgb(253, 246, 236)
        note over SVC: fallbackDecision refuses when falling back would be wrong:<br/>cause not errs.SourceUnusable (404, 400, 403), consistency=strong,<br/>fallback none or equal to primary, BOTH whose required side failed,<br/>or a fallback that is itself unhealthy.
        SVC->>SVC: cause is SourceUnusable, consistency=bounded,<br/>fallback=EXECUTION and it is healthy -> proceed
        SVC->>SVC: execution_fallback_total{trigger="call_failure"}++<br/>log WARN "preferred source failed; falling back"
    end

    SVC->>EDS: GET /resources/R007/latest-execution
    EDS-->>SVC: execution record
    SVC-->>UI: 200 OK<br/>meta.routingDecision=EXECUTION<br/>meta.routingRule=fallback.primary_failed<br/>meta.degraded=true<br/>warnings=[SOURCE_UNAVAILABLE]

    note over BRK: Once enough failures accumulate<br/>(failure_threshold 0.5 over minimum_requests 20 in a 30s window)<br/>the breaker opens and subsequent requests take the<br/>*pre-flight* path instead: rule health.primary_unavailable,<br/>and no doomed call is issued at all.
```

**What it is showing.** The twelfth rule id, `fallback.primary_failed`, and why it
has to exist even though rule 7 already handles an unhealthy primary.

**Why pre-flight health routing is not enough.** The router's health snapshot comes
from the resilience layer — breaker state and bulkhead saturation — read locally,
with no network call, because the router consults it on every request. That
snapshot can only describe an outage the breaker has already *learned about*. The
first failures of any outage necessarily arrive before the breaker has seen enough
of them to trip: with `minimum_requests: 20` and `failure_threshold: 0.5` over a 30s
window, at least ten failures land before rule 7 can help. Without a call-time
fallback, those requests fail even though a perfectly healthy fallback source was
sitting there. So the configured fallback travels on every decision, whether or not
the rule that fired used it, and the application layer retries against it after a
real failure.

**Where it deliberately refuses.** `fallbackDecision` returns false — no fallback,
propagate the original error — in five cases, each because falling back would be
*wrong* rather than merely unhelpful:

The gate is `errs.SourceUnusable`, not `errs.IsDegradable`, and the distinction is
deliberate. `SourceUnusable` is the wider predicate: it means "the source that
produced this error cannot serve this request, but a different source might".
`UPSTREAM_TIMEOUT`, `UPSTREAM_UNAVAILABLE`, `UPSTREAM_INVALID_RESPONSE` and
`SCHEMA_VERSION_MISMATCH` all qualify. The last two are *terminal* rather than
degradable — retrying cannot help, and no amount of waiting makes an incompatible
contract compatible — but they are still a reason to try the other source, in
exactly the way a timeout is. `NOT_FOUND` and the other client faults do not
qualify: asking a second source about a resource the first authoritatively does
not have would turn a correct answer into a wrong one.

| Condition | Why refusing is correct |
|---|---|
| The cause is not `errs.SourceUnusable` (`404`, `400`, `403`, `INTERNAL`) | A second source cannot fix a client's mistake, and a 404 from the ODS is an answer, not a failure |
| `consistency = strong` | The caller asked for one specific source's live answer and must not silently receive another's |
| `fallback` is `none`, or equals the primary | There is nowhere to go |
| Target was `BOTH` and the fallback is `execution` | The side that failed was the required one; the fallback names the side that is already missing |
| The fallback source is itself unhealthy | Issuing a call that is certain to fail wastes the caller's remaining budget |

When the fallback does run and *also* fails, the **primary's** error is what gets
reported — the fallback's is logged at warn and discarded. That is not merely a
preference for the more informative message: the fallback's error describes a
source the caller never asked about, and a `NOT_FOUND` from the execution source
means only that the resource has no execution history. Returning it would turn a
transient operational outage into a `404`, and — because `resourceView` negatively
caches a `NOT_FOUND` — would then cache that `404` for `negative_ttl` against a
resource that plainly exists. Reporting the primary's error is what keeps a
fallback-path `NOT_FOUND` out of the negative cache entirely.

**A fallback answer that cannot cover the request is a `206`.**
`targetMissesRequiredFields` checks each required field against the catalogue: a
field is unsatisfiable only when **none** of its suppliers is in the chosen
target. So `/resources/{id}` (`resource_read`) answered by the EDS is a `206`
with a `PARTIAL_DATA` warning, because the execution source holds no
`configuration`, `owner`, `metrics` or `topology`; `/status` answered by the EDS
stays `200`, because `status` lists the execution source as a supplier. Testing
against each field's *most authoritative* supplier instead would flag every
fallback answer as partial, including ones the fallback can legitimately serve.

---

## 8. Request path: stale-cache degradation

```mermaid
sequenceDiagram
    autonumber
    actor UI as Console
    participant SVC as application.Service
    participant CA as cache.Manager
    participant R as router
    participant ODS as Operational Data Source
    participant EDS as Execution Data Source

    note over CA: an entry for this key was stored 40s ago with<br/>cache_ttl 3s and physical lifetime 3s + stale_grace 5m,<br/>so it is logically expired but still physically resident

    UI->>SVC: GET /resources/R007/status (tenant local)
    SVC->>CA: GetOrLoad(key, 3s)
    CA->>CA: Get -> entry.Expired(now) = true
    CA-->>SVC: miss (the ordinary read path cannot see expired entries)

    SVC->>R: Select(...)
    R-->>SVC: Target=NONE, Rule=health.both_unavailable<br/>reason: operational is CIRCUIT_OPEN and execution is CIRCUIT_OPEN
    SVC-->>SVC: NO_SOURCE_AVAILABLE

    rect rgb(253, 240, 240)
        note over SVC,CA: serveStale — a policy decision, never a convenience
        SVC->>SVC: cause is NO_SOURCE_AVAILABLE -> eligible
        SVC->>SVC: rule.AllowStale for resource_status = true<br/>(tenant globex has allow_stale:false and gets a 503 here)
        SVC->>SVC: classification consistency != strong
        SVC->>CA: GetStale(key)
        CA-->>SVC: entry (not negative) + layer L2
        SVC->>SVC: EffectiveFreshness(now).Age = 40s <= max_stale 120s
        SVC->>SVC: stale_response_total++, log WARN with age_seconds
    end

    SVC-->>UI: 200 OK<br/>meta.routingDecision=NONE<br/>meta.routingRule=degrade.stale_cache<br/>meta.degraded=true<br/>meta.cache={hit:true, layer:"L2", ageMs:40000}<br/>meta.sources=[CACHE, OPERATIONAL]<br/>warnings=[STALE_DATA]

    note over SVC,UI: If any guard had failed — allow_stale false, age past<br/>max_stale, strong consistency, no entry, or a caller-caused<br/>error — the answer is 503 NO_SOURCE_AVAILABLE listing<br/>both source states, retryable: true.
```

**What it is showing.** The last rung of the ladder, and the mechanism that makes
it possible: a cache entry has **two** lifetimes.

| Lifetime | Value | Enforced by | Visible to |
|---|---|---|---|
| Logical (`CacheTTL`) | the request type's `cache_ttl` (3s for `resource_status`) | `Manager.Get` via `entry.Expired(now)` | the ordinary read path; reported in the envelope |
| Physical | `cache_ttl + cache.stale_grace` (3s + 5m) | the backend's own TTL, set in `Manager.store` | only `Manager.GetStale` |

`GetStale` is a separate method rather than a flag on `Get`, so no ordinary read
path can reach expired data by accident. Reaching it requires four things at once:
an `errs.SourceUnusable` cause (or `NO_SOURCE_AVAILABLE`), `allow_stale` on the
request type, a consistency level below `strong`, and an entry whose effective age
is within `max_stale`.

The cause predicate is deliberately the **same one the call-time fallback uses**.
The stale rung sits below the fallback rung on the ladder, so anything that
justified trying another source must also justify serving an old answer once that
source is gone too; using a narrower predicate here would leave a hole where a
request could fall past a fallback it was allowed to take and then be refused a
cached answer it was also allowed to take.

A stale-served response also **carries the cached entry's own `provenance`,
`warnings` and `partial` flag forward**. A stale serve is exactly when a caller
most needs to know where each field came from, and dropping the flag would
silently turn a partial answer back into an apparently complete one.

**This is where tenant configuration becomes observable behaviour.** Tenant `local`
inherits `resource_status.allow_stale: true, max_stale: 120s` and gets the degraded
`200` above. Tenant `globex` overrides `allow_stale: false` and gets a `503` for
the identical request against the identical cache state — which is the point:
"prefer correctness over latency" is four lines of YAML, not a code path.

**Degraded *and partial* answers are cached briefly.** `Manager.store` clamps the
TTL of an entry marked `Degraded` **or** `Partial` to `negative_ttl` (3s), so the
degradation cannot outlive the incident that produced it. Partial matters as much
as degraded: an answer missing its execution half would otherwise be replayed to
every caller for the full cache TTL, long after the execution source came back.

**A cache hit can report STALE.** Cached entries carry their own freshness,
provenance and warnings. On a hit, `EffectiveFreshness(now)` re-reports the stored
freshness with the accumulated cache age added, so an entry that aged past its
freshness TTL while sitting in the cache is reported as `STALE` and `degraded`,
with a warning, even though the cache hit itself was perfectly valid. The cache is
never permitted to assert freshness on its own authority.

**An `UNKNOWN` entry stays `UNKNOWN`.** `EffectiveFreshness` re-derives `FRESH` or
`STALE` from the accumulated age only when the stored verdict was one of those two.
Ageing an `UNKNOWN` into `STALE` would look conservative but is actually a loss of
information: the router treats the two differently on purpose (rule 9 versus rule
10), and a cache hit must not manufacture a verdict the original read never
produced.

---

## 9. Circuit breaker states

```mermaid
stateDiagram-v2
    direction LR

    [*] --> Closed

    Closed: Closed
    Closed: calls pass through
    Closed: outcomes land in a ring of 10 buckets over window=30s
    Closed: so a burst of failures ages out gradually
    Closed: circuit_breaker_state = 0

    Open: Open
    Open: Allow() returns CIRCUIT_OPEN without calling the source
    Open: rejections are counted in the window
    Open: Health.Detail = CIRCUIT_OPEN so the router sees it pre-flight
    Open: circuit_breaker_state = 2

    HalfOpen: HalfOpen
    HalfOpen: at most half_open_max_calls=3 concurrent probes admitted
    HalfOpen: further callers rejected as CIRCUIT_OPEN
    HalfOpen: Health.Detail = CIRCUIT_HALF_OPEN and the source counts as available
    HalfOpen: circuit_breaker_state = 1

    Closed --> Open: failure ratio >= failure_threshold 0.5 over >= minimum_requests 20 in 30s
    Open --> HalfOpen: open_timeout 5s elapsed (evaluated lazily on the next State read)
    HalfOpen --> Closed: half_open_successes=2 consecutive successes, window cleared on close
    HalfOpen --> Open: any single failure while probing
    Closed --> Closed: success, or failure below the threshold or below minimum_requests

    note right of Closed
        ABSTAIN — neither success nor failure — because they are
        not evidence about the source's health in EITHER
        direction: NOT_FOUND, INVALID_REQUEST, FORBIDDEN,
        UNAUTHENTICATED, TENANT_MISMATCH, and
        SCHEMA_VERSION_MISMATCH, where the source is up, fast
        and correct and the BFF simply does not understand its
        contract. Recording them as successes would let a source
        answering nothing but 404s while genuinely down satisfy
        the half-open threshold and be re-admitted to full
        traffic. Timeouts and transport failures do count.
    end note

    note right of Open
        Every transition emits circuit_breaker_transition_total
        with from and to attributes, fired outside the mutex.
    end note
```

**What it is showing.** One breaker per source, with the real numbers from
`configs/bff.yaml` (identical for both sources today).

**Four design choices, each fixing a specific failure mode.**

| Choice | Failure mode it prevents |
|---|---|
| Rolling window as a ring of 10 buckets, not a counter pair | A breaker that clears its whole window on a tick boundary flaps: it forgets an outage wholesale and re-admits full traffic |
| `minimum_requests: 20` gate on the ratio | Without it, the first failed call after a quiet period trips at a 100% failure ratio |
| Half-open admits at most 3 probes and needs 2 consecutive successes | One lucky call must not re-admit full traffic to a source that is still unwell |
| Client-caused errors **abstain** — `Breaker.Do` returns without calling `Record` at all | A burst of 404s from a UI polling a deleted resource must not open the circuit on a healthy source. Abstaining rather than recording a success is the symmetric half: a source answering nothing but 404s while it is genuinely down would otherwise accumulate "successes", satisfy the half-open threshold and be re-admitted to full traffic |
| `bucketIndex` clamps at zero | A backwards wall-clock step, under an injected clock that has dropped its monotonic reading, would otherwise produce a negative ring index and panic inside a held mutex |
| A schema-version mismatch abstains too | The source is up, fast and answering correctly; the BFF just does not understand the contract it is speaking. Counting it as a health failure would trip the breaker on a healthy source, quietly reroute every request to the slower one, and replace a loud version-incompatibility alert with a vague availability one. It surfaces through `schema_version_mismatch_total`, through a `502` where no fallback exists, and through the call-time fallback where one does |

**How the breaker reaches routing.** `Executor.Healthy()` is false when the breaker
is open or the bulkhead is saturated, and `HealthDetail()` renders the reason as
`CIRCUIT_OPEN`, `CIRCUIT_HALF_OPEN`, `SATURATED`, `HEALTHY` or `UNCONFIGURED`.
That string is what appears in the routing decision's `Reason` and in the `sources`
array of an error document, so an operator reads the cause rather than inferring it.
Note that a half-open source still counts as *available* for routing: the router
will select it, and the breaker's own probe budget is what limits the exposure.

**The lazy transition.** `Open → HalfOpen` is evaluated inside `State()`, not by a
timer. There is no background goroutine per breaker, and a breaker on a source
nobody is calling costs nothing.

---

## 10. The full routing rule chain

```mermaid
flowchart TD
    S([Select: classification + frozen health snapshot + now]) --> G0

    G0{"0 guard.unconfigured_request_type<br/>is a routing rule configured<br/>for this request type?"}
    G0 -- no --> DG0["Target=NONE<br/>503 NO_SOURCE_AVAILABLE<br/>emitted to routing_decision_total"]
    G0 -- yes --> R1

    R1{"1 guard.tenant_missing<br/>tenant id blank?"}
    R1 -- yes --> D1["Target=NONE<br/>400 INVALID_REQUEST"]
    R1 -- no --> R2

    R2{"2 health.both_unavailable<br/>neither source available?"}
    R2 -- yes --> D2["Target=NONE<br/>application layer tries the stale ladder"]
    R2 -- no --> R3

    R3{"3 fields.execution_only<br/>every required field has exactly<br/>one supplier and it is EXECUTION?"}
    R3 -- "yes, EDS healthy" --> D3["PIN preferred=EXECUTION,<br/>clear the configured fallback,<br/>then TERMINATE<br/>Target=EXECUTION"]
    R3 -- "yes, EDS unhealthy" --> D3b["Target=NONE<br/>nothing to fall back to"]
    R3 -- no --> R4

    R4{"4 fields.operational_only<br/>same test for OPERATIONAL?"}
    R4 -- "yes, ODS healthy" --> P4["PIN preferred=OPERATIONAL,<br/>clear the configured fallback,<br/>then CONTINUE the chain<br/>(so the max_stale ceiling still applies)"]
    R4 -- "yes, ODS unhealthy" --> D4b["Target=NONE<br/>rule id fields.operational_only"]
    R4 -- no --> R5
    P4 --> R5

    R5{"5 fields.span_both<br/>required fields need both sources?"}
    R5 -- both healthy --> D5["Target=BOTH"]
    R5 -- only ODS healthy --> D5a["Target=OPERATIONAL"]
    R5 -- only EDS healthy --> D5b["Target=EXECUTION"]
    R5 -- no --> R6

    R6{"6 consistency.strong_requires_operational<br/>consistency = strong?"}
    R6 -- yes, preferred healthy --> D6["Target=preferred source, read live<br/>AllowStale forced false"]
    R6 -- yes, preferred unhealthy --> D6b["Target=NONE"]
    R6 -- no --> R7

    R7{"7 health.primary_unavailable<br/>preferred source unavailable?"}
    R7 -- yes, healthy fallback --> D7["Target=fallback<br/>execution_fallback_total++"]
    R7 -- yes, preferred=both, one side up --> D7b["Target=the healthy side"]
    R7 -- yes, nothing left --> D7c["Target=NONE"]
    R7 -- no --> R8

    R8{"8 ttl.operational.fresh<br/>preferred=operational and<br/>probe verdict FRESH?"}
    R8 -- yes --> D8["Target=OPERATIONAL<br/>operational_ttl_hit_total++<br/>EDS never contacted"]
    R8 -- no --> R9

    R9{"9 ttl.operational.stale<br/>verdict STALE?"}
    R9 -- "healthy fallback exists" --> D9a["Target=fallback<br/>operational_ttl_miss_total++<br/>execution_fallback_total++"]
    R9 -- "no fallback, allow_stale, age <= max_stale" --> D9b["Target=OPERATIONAL<br/>degraded=true, warning STALE_DATA"]
    R9 -- "no fallback, stale forbidden or past max_stale" --> D9c["Target=NONE"]
    R9 -- not stale --> R10

    R10{"10 ttl.unknown_freshness<br/>verdict UNKNOWN?<br/>probe failed or disabled"}
    R10 -- "on_unknown_freshness = none" --> D10n["Target=NONE<br/>the operator chose to fail<br/>rather than guess"]
    R10 -- "configured source healthy" --> D10["Target=that source<br/>default: operational<br/>(an UNPARSEABLE value falls back<br/>to operational; 'none' does not)"]
    R10 -- "it is down, other side healthy,<br/>and no field-rule pinned the set" --> D10b["Target=the other source"]
    R10 -- "pinned, or neither side up" --> D10c["Target=NONE"]
    R10 -- no --> R11

    R11["11 default.preferred_source<br/>unconditional chain terminator"]
    R11 --> D11["Target = preferred_source<br/>operational | execution | both"]

    D3 --> DISPATCH
    D5 --> DISPATCH
    D5a --> DISPATCH
    D5b --> DISPATCH
    D6 --> DISPATCH
    D7 --> DISPATCH
    D7b --> DISPATCH
    D8 --> DISPATCH
    D9a --> DISPATCH
    D9b --> DISPATCH
    D10 --> DISPATCH
    D10b --> DISPATCH
    D11 --> DISPATCH

    DISPATCH{{"dispatch: aggregation.FanOut over the selected sources"}}
    DISPATCH -- required source failed --> FB{"12 fallback.primary_failed — POST-ROUTING<br/>stamped by the application layer, not by the chain:<br/>errs.SourceUnusable(cause), consistency not strong,<br/>a different fallback configured and healthy?"}
    FB -- yes --> FBY["re-fetch against the fallback<br/>degraded=true, warning SOURCE_UNAVAILABLE<br/>naming the source that FAILED<br/>execution_fallback_total{trigger=call_failure}++<br/>206 partial if the fallback cannot<br/>supply every requested field"]
    FB -- no --> LADDER
    D1 --> ERR([error envelope])
    DG0 --> LADDER
    D10n --> LADDER
    D2 --> LADDER
    D3b --> LADDER
    D4b --> LADDER
    D6b --> LADDER
    D7c --> LADDER
    D9c --> LADDER
    D10c --> LADDER

    LADDER{"degrade.stale_cache<br/>allow_stale, entry within max_stale,<br/>consistency not strong?"}
    LADDER -- yes --> STALE["200 with degraded=true<br/>stale_response_total++"]
    LADDER -- no --> S503["503 NO_SOURCE_AVAILABLE<br/>or the upstream error's own status"]

    classDef terminal fill:#eef7ee,stroke:#4b7a4b,color:#1b2733
    classDef bad fill:#fdf0f0,stroke:#a05252,color:#1b2733
    classDef pin fill:#f5f0fa,stroke:#6b5b8a,color:#1b2733
    class D8,D3,D5,D11 terminal
    class P4 pin
    class D1,D2,D3b,D4b,D6b,D7c,D9c,D10c,D10n,DG0,S503 bad
```

Three nodes in that picture are not ordinary rules.

**Rules 3 and 4 both pin.** Pinning fixes the source set *and clears the
configured fallback*, because a fallback source that cannot supply the requested
fields is not a fallback, it is a wrong answer — and `finish()` would otherwise
re-attach the configured one. A pin also binds rule 10, which may not cross it to
reach the other source. Where they differ is what happens next: rule 3
terminates, while **rule 4 re-enters the chain**, so every operational-only
request type is still decided by rules 8, 9 or 10 and is still subject to the
`max_stale` ceiling.

**`guard.unconfigured_request_type` is a pre-chain exit**, not a rule. It fires
when the request type has no routing rule at all — a deployment error rather than
a routing outcome — and it is reported to `routing_decision_total` like every
other decision, because a configuration that has lost a request type should be
visible in the metric an operator reads first rather than only in a log line.

**`fallback.primary_failed` is not in the chain at all**: it is stamped after
dispatch, by the application layer, once a source call has actually failed.

### 10.1 The rules, in order

| # | Rule id | Fires when | Produces |
|---|---|---|---|
| 1 | `guard.tenant_missing` | No resolved tenant on the classification | `NONE` → `400` |
| 2 | `health.both_unavailable` | Neither source available | `NONE` → the stale ladder |
| — | `guard.unconfigured_request_type` | **Before the chain runs**: the request type has no routing rule at all | `NONE` → `503`. Emitted to `routing_decision_total`; a non-zero rate is a deployment defect, not a routing outcome |
| 3 | `fields.execution_only` | Every required field has exactly one supplier, and it is the EDS | **Pins** the source to the EDS and clears the configured fallback, then terminates: `EXECUTION`, or `NONE` if the EDS is down |
| 4 | `fields.operational_only` | Same test for the ODS | Pins the same way, but *continues* the chain instead of terminating — so rules 8/9/10 still decide and the `max_stale` ceiling still applies. Emits its own id only when the ODS is down, and then as `NONE` |
| 5 | `fields.span_both` | The required fields' primary suppliers include both sources | `BOTH`, or the healthy side alone |
| 6 | `consistency.strong_requires_operational` | `consistency = strong` | Live read of the preferred source, `AllowStale` forced false |
| 7 | `health.primary_unavailable` | The preferred source is unavailable pre-flight. `preferred_source: both` is matched on the configured *string*, since "both" is not a `SourceKind` and parses to none | The configured fallback, the healthy half of a `both`, or `NONE`. `degraded: true` with a `SOURCE_UNAVAILABLE` warning naming the source that failed |
| 8 | `ttl.operational.fresh` | Preferred is operational and the probe says `FRESH` | `OPERATIONAL`; the EDS is not contacted |
| 9 | `ttl.operational.stale` | Preferred is operational and the probe says `STALE` | Fallback first, then stale-serve, then `NONE` |
| 10 | `ttl.unknown_freshness` | Preferred is operational and the verdict is `UNKNOWN` | `routing.defaults.on_unknown_freshness`. `none` is **honoured** and yields `NONE`; only an *unparseable* value falls back to `operational`. Crossing to the other side is allowed only when no field rule pinned the set |
| 11 | `default.preferred_source` | Always — the terminator | The configured `preferred_source` |
| — | `fallback.primary_failed` | **Not in the chain.** Stamped after dispatch when the preferred source actually failed, gated on `errs.SourceUnusable` | The configured fallback, `degraded: true`, warning `SOURCE_UNAVAILABLE` |
| — | `degrade.stale_cache` | **Not in the chain.** Stamped by `Service.serveStale` when every source refused and an expired-but-resident entry passes the four gates | `NONE`, `degraded: true`, warning `STALE_DATA` |

### 10.2 Which rule each endpoint actually hits

The chain is data-driven, so the rule a given endpoint fires is a consequence of
the field catalogue in `precedence.fields`, not of endpoint-specific code. With the
shipped configuration:

| Endpoint | Required fields | Exclusive to one source? | Rule that fires (healthy sources) |
|---|---|---|---|
| `/status` | `status` | No — `status` lists both sources | 8, 9 or 10 depending on the probe verdict |
| `/configuration` | `configuration` | Yes, operational | **4 pins**, then 8, 9 or 10 decides |
| `/resources/{id}` | `status`, `subState`, `configuration`, `owner`, `metrics`, `topology`, `labels`, `customerId` | No | 8, 9 or 10 |
| `/details` | the above plus `latestExecution`, `executionHistory` | No — spans both | **5** |
| `/executions` | `executionHistory` | Yes, execution | **3** |
| `/executions/{id}` | `executionStatus`, `workflowSteps` | Yes, execution | **3** |

`/status` asks for `status` alone. `subState` was deliberately dropped from the
required set: it is `omitempty` decoration that no caller can depend on, and
listing it as required would make every answer from a source that does not model
it look incomplete — which, now that an under-covered fallback answer is a `206`,
would turn a perfectly good execution-sourced status into a partial response.

Two consequences are worth stating plainly because they are easy to get wrong when
reading the policy text alone.

`/configuration` normally emits a TTL rule id — `ttl.operational.fresh` or
`ttl.operational.stale` — not `fields.operational_only`. Rule 4 *pins* rather than
terminates: it sets the preferred source to the ODS, clears the configured
fallback, and returns `handled=false` so evaluation continues.

The reason is a safety property, not an optimisation. Terminating at rule 4 would
skip the TTL rules, and with them the `max_stale` ceiling — so `resource_configuration`,
whose fields no other source holds, would happily serve a record of any age. The
ceiling has to hold precisely for the request type that has nowhere else to go.
Pinning also clears the fallback, because a fallback source that cannot supply the
requested fields is not a fallback, it is a wrong answer; rules 8/9/10 therefore
see `preferred = operational, fallback = none` and will serve the record, serve it
as stale-but-permitted, or refuse.

Rule 4 emits its own id in exactly one case: the ODS is unavailable, in which case
it terminates with `Target = NONE` and the ladder takes over.

Rule 3, `fields.execution_only`, pins in exactly the same way and for the same
reason — it clears the configured fallback so `finish()` cannot re-attach a source
that lacks the requested fields — but it *terminates* on both branches. TTL
semantics belong to the operational source, so once the answer can only come from
the EDS there is nothing further for the chain to evaluate.

Rule 11 is unreachable for all six shipped request types. Every one of them is
caught by rules 3, 5, 8, 9 or 10 (rule 4 pins but never terminates on the healthy
path). Rule 11 exists to make the chain *total*: a `Decision` with an empty `Rule`
is a defect, and the terminator is what makes that impossible even if a future
request type arrives with no freshness semantics.

### 10.3 Why a chain rather than nested conditionals

The decision space is roughly `6 request types × 3 freshness verdicts × 3 health
states² × 3 consistency levels × 2 allow_stale × 3 fallback settings`. As nested
conditionals across six handlers, that is hundreds of reachable paths with nothing
preventing two handlers from disagreeing. As a chain it is eleven predicates
evaluated once each, and:

- **every decision is attributable** — the rule id is a metric attribute, a span
  attribute and a response field, so "why did we call the slow source a thousand
  times more this hour?" is one `group by routing_rule` query;
- **the whole policy is a truth table** — the rules are pure functions of a value
  struct, so the tests need no HTTP, no gRPC and no clock;
- **tenant variation is an overlay, not a branch** — `tenants.acme` gets
  `resource_status.ttl: 5s` from four lines of YAML;
- **adding a rule is an insertion** — a new `cost.budget_exceeded` rule is one
  position in the chain plus one row in the truth table.

What is deliberately *not* configurable is the chain's **order**. Configuration
varies the parameters of the rules; the code owns their sequence. Making order
configurable would let an operator build an incoherent policy — freshness evaluated
before the tenant guard — and would make the truth tables unverifiable.

---

## 11. Deployment on AWS

```mermaid
flowchart TB
    client([Console / API client])

    subgraph aws["AWS account, one region"]
        r53["Route 53 record"]
        acm["ACM certificate<br/>TLS 1.3 / TLS 1.2 policy"]

        subgraph vpc["VPC"]
            subgraph pub["Public subnets, one per AZ"]
                alb["Application Load Balancer<br/>ttl-aware-bff-alb<br/>internet-facing, target-type ip<br/>listeners 80 redirect to 443<br/>health check GET :9090/readyz"]
            end

            subgraph priv["Private subnets, one per AZ"]
                subgraph eks["EKS cluster — namespace bff"]
                    subgraph ng["Managed node group (min/desired/max from tfvars)"]
                        p1["Pod ttl-aware-bff<br/>container port 8080 http, 9090 admin<br/>livez / readyz / healthz on the admin port"]
                        p2["Pod ttl-aware-bff"]
                        p3["Pod ttl-aware-bff"]
                        adot["ADOT collector<br/>OTLP :4317"]
                    end
                    svc["Service ttl-aware-bff<br/>8080 -> http, 9090 -> admin"]
                    hpa["HorizontalPodAutoscaler<br/>min 3, max 30, CPU target 70%"]
                    pdb["PodDisruptionBudget"]
                    np["NetworkPolicy<br/>admin port reachable only in-cluster"]
                    sm["ServiceMonitor<br/>scrapes :9090/metrics"]
                    sa["ServiceAccount + IRSA role"]
                end

                redis[("ElastiCache for Redis<br/>replication group, Multi-AZ,<br/>automatic failover<br/>L2 response cache")]
                ods["Operational Data Source<br/>gRPC, existing platform service"]
                eds["Execution Data Source<br/>REST, existing platform service"]
            end
        end

        cw["CloudWatch logs and metrics"]
        xray["Trace backend via OTLP"]
        ecr["ghcr.io images<br/>ttl-aware-bff, -opsource, -exsource"]
        sm2["Secrets: JWT / Redis auth<br/>via env, referenced by name only"]
    end

    client --> r53 --> alb
    acm -.-> alb
    alb -->|"HTTP to pod IPs, port 8080"| svc
    svc --> p1
    svc --> p2
    svc --> p3
    hpa -.->|scales| ng
    pdb -.->|guards evictions| ng
    np -.->|restricts| ng
    sm -.->|scrape| svc

    p1 -->|gRPC| ods
    p1 -->|HTTPS| eds
    p1 -->|"Redis, TLS optional"| redis
    p2 --> redis
    p3 --> redis
    p1 -->|OTLP| adot
    adot --> cw
    adot --> xray
    sa -.->|IRSA, no static keys| redis
    ecr -.->|image pull| ng
    sm2 -.-> p1

    classDef ext fill:#eef2f7,stroke:#5b6b7f,color:#1b2733
    class ods,eds,cw,xray,ecr,acm,r53,sm2 ext
```

**What it is showing.** The Terraform stack (`deploy/terraform`) provisions the VPC,
the EKS cluster, the ElastiCache replication group, the ALB, the IAM/IRSA roles and
the observability wiring. The Kubernetes manifests (`deploy/k8s`, and the equivalent
Helm chart in `deploy/helm/ttl-aware-bff`) provision the workload.

**Four numbers that have to agree, and where they come from.**

| Constraint | Values | Where |
|---|---|---|
| `preStop` sleep + `server.shutdown_grace` < `terminationGracePeriodSeconds` | 10s + 25s < 45s | `configs/bff.yaml`, `deploy/k8s/deployment.yaml` |
| `server.request_timeout` > the slowest per-source budget, and comfortably < the ALB idle timeout | 8s > 2s | `configs/bff.yaml` |
| `freshness_probe_timeout` < `call_timeout` | 120ms < 400ms | enforced by config validation |
| HPA `minReplicas` = Deployment `replicas`, and the PDB stays satisfiable | 3 = 3 | `deploy/k8s/hpa.yaml`, `deployment.yaml`, `pdb.yaml` |

**Why the ALB health-checks `:9090/readyz` and not `:8080`.** Readiness answers
"should traffic be sent here?", and it deliberately does **not** fail on a source
outage: with both sources down the BFF can still serve stale cached data, and
removing every pod from the load balancer would turn a degraded service into no
service. It reports the source states in its body for humans, and returns `200`
with `status: degraded`. Liveness is stricter about its independence: it depends on
no external system at all, because a source outage is not a reason for the kubelet
to restart the process, and a liveness probe that checks dependencies converts a
dependency blip into a cluster-wide restart storm.

**Why the admin port is not on the ingress.** `/metrics`, `/buildinfo` and
`/config/routing` are unauthenticated. The `NetworkPolicy` is what keeps them
cluster-internal; the ingress only ever routes `:8080`.

---

## 12. Failure modes

Every row below is a behaviour with an implementation, a signal and a defined HTTP
result. "Signal" is what you would query or grep to confirm the mechanism fired.

| # | Failure | Mechanism that handles it | Observable signal | HTTP result |
|---|---|---|---|---|
| 1 | Operational record inside its TTL | Rule 8 `ttl.operational.fresh`; EDS not contacted | `operational_ttl_hit_total++`; `routing_decision_total{routing_rule="ttl.operational.fresh"}`; `meta.freshness.state=FRESH` | `200` |
| 2 | Operational record past its TTL, fallback configured and healthy | Rule 9 branch (a) → EDS | `operational_ttl_miss_total++`, `execution_fallback_total++`; `meta.routingRule="ttl.operational.stale"` | `200` |
| 3 | Past TTL, no usable fallback, `allow_stale`, within `max_stale` | Rule 9 branch (b) → stale operational read | `meta.degraded=true`, warning `STALE_DATA`, `X-BFF-Degraded: true` | `200` |
| 4 | Past TTL, `allow_stale: false` or age past `max_stale` | Rule 9 branch (c) → ladder | `routing_decision_total{routing_decision="NONE"}` | `503 NO_SOURCE_AVAILABLE` |
| 5 | Freshness probe times out or errors | Verdict `UNKNOWN` → rule 10 → `on_unknown_freshness`. `none` is honoured and yields `NONE`; only an unparseable value falls back to `operational`. A pin from rule 3/4 forbids crossing to the other source | `router.Hooks.OnProbeErr`; `meta.freshness.state=UNKNOWN` | `200`, or `503` under `none` |
| 6 | ODS breaker already open at routing time | Rule 7 `health.primary_unavailable` → configured fallback | `execution_fallback_total++`; `circuit_breaker_state{source="operational"}=2`; warning `SOURCE_UNAVAILABLE` naming **OPERATIONAL**, the source that failed | `200` degraded, or `206` when the fallback cannot supply every requested field (`/resources/{id}`) |
| 7 | ODS fails on the call, breaker still closed | `fallback.primary_failed` in the application layer | `execution_fallback_total{trigger="call_failure"}++`; WARN "preferred source failed; falling back" | `200` degraded, or `206` when the fallback cannot supply every requested field |
| 8 | ODS unavailable and `fallback: none` (`/configuration`) | Ladder: fresh cache → stale cache → refuse | `stale_response_total++` if stale-served | `200` degraded, else `503` |
| 9 | EDS unavailable, request type is execution-only | Rule 3 with an unhealthy EDS → `NONE`; no operational substitution | `datasource_error_total{source="EXECUTION"}` | `503 UPSTREAM_UNAVAILABLE` |
| 10 | EDS unavailable, `/details` (execution optional) | Fan-out marks the task optional; response is partial | `partial_response_total++`; warning `SOURCE_UNAVAILABLE`; `meta.partial=true` | `206` |
| 11 | EDS exceeds its per-source timeout on `/details` | Task's own derived context expires; other tasks continue | Warning `SOURCE_TIMEOUT`; `TaskResult.TimedOut` | `206` |
| 12 | Required source times out | `UPSTREAM_TIMEOUT` → call-time fallback → ladder. If the fallback also fails, the **primary's** error is reported and a fallback-path `NOT_FOUND` is never negatively cached | `datasource_error_total{error_code="UPSTREAM_TIMEOUT"}` | `504`, or `200`/`206` if the ladder catches it |
| 13 | Both sources unavailable, usable cache entry, `allow_stale` | `degrade.stale_cache` via `Manager.GetStale` within `stale_grace` | `stale_response_total++`; `meta.routingRule="degrade.stale_cache"`; `meta.cache.hit=true` | `200` degraded |
| 14 | Both sources unavailable, no usable entry or stale forbidden | Ladder exhausted | `routing_decision_total{routing_rule="health.both_unavailable"}` | `503 NO_SOURCE_AVAILABLE`, `retryable: true` |
| 15 | Cached answer aged past its freshness TTL | `EffectiveFreshness(now)` re-evaluates on every hit | `meta.freshness.state=STALE` with `meta.cache.hit=true`; warning `STALE_DATA` | `200` degraded |
| 16 | N concurrent identical misses | `singleflight` per key, plus an optional Redis lock across processes | One upstream read; `Result.Stampede=true` for the waiters | `200` for all N |
| 17 | Cache backend itself fails | `cache.fail_open: true` — an error is reported as a miss | `cache_error_total++` | unaffected |
| 18 | Resource missing from every consulted source | The guard tests the **fetched inputs**, not the merged output — `Merge` stamps the tenant and resource id first, so a guard on the merged record could never fire and "every source answered with nothing" would be served as a `200` with an empty body. 404 propagates; negatively cached for `negative_ttl: 3s` | `cache` holds a negative entry | `404 NOT_FOUND` |
| 19 | Source returns a malformed record | Adapters and the mapper reject it rather than passing it through | `datasource_error_total{error_code="UPSTREAM_INVALID_RESPONSE"}` | `502 UPSTREAM_INVALID_RESPONSE` |
| 19a | Source declares a schema version the BFF does not accept | **The breaker is untouched** — the source is healthy, only its contract is unintelligible. `errs.SourceUnusable` is true, so the call-time fallback runs where one is configured | `schema_version_mismatch_total`; **no** `circuit_breaker_transition_total` | `200` degraded via the fallback, else `502 SCHEMA_VERSION_MISMATCH` |
| 20 | Source clock disagrees with the BFF's | Age computed in the source's own clock domain (`server_time − last_updated`); fallback arithmetic biased toward "older" | `clock_skew_detected_total++`; `meta.freshness.skewCorrected=true`; warning `CLOCK_SKEW_DETECTED` | unaffected |
| 21 | Both sources supply a field with different values | `SourcePrecedencePolicy` picks by configured order, never by recency | `precedence_conflict_total++`; warning `CONFLICT_RESOLVED`; `meta.provenance` names the winner | `200` |
| 22 | Workflow running; operational record lags behind it | `execution_overrides_when_running: [status, subState]`, plus `resolve_in_flight_execution` so `/status` sees the same execution `/details` does. The override needs an execution candidate that is *actually* `InProgress`; a stale `in_flight_execution_ref` alone does not confer it | `meta.provenance.status="EXECUTION"`; span `bff.resolve_in_flight` | `200` |
| 23 | In-flight execution lookup itself fails | Best-effort: the operational answer stands, logged at debug | span status "in-flight execution lookup failed; operational answer stands" | `200`, unchanged |
| 24 | A slow source consumes the BFF's concurrency | Bulkhead per source (`operational: 256`, `execution: 64`) | `bulkhead_in_flight`, `bulkhead_rejected_total`; `HealthDetail = SATURATED` | `503`, or degraded via the ladder |
| 25 | A single tenant floods the API | Rate limit, per tenant (`rps: 200`, `burst: 400`; `acme` overrides to 500/1000) | `rate_limited_total++` | `429 RATE_LIMITED` |
| 26 | Request has no resolved tenant | Rule 1 `guard.tenant_missing` — fail closed, no default tenant | `routing_decision_total{routing_rule="guard.tenant_missing"}` | `400 INVALID_REQUEST` |
| 27 | Caller asserts a tenant its token does not carry | `ResolveTenant` refuses rather than resolving in either direction | `TENANT_MISMATCH` in the error document | `403` |
| 28 | A configuration reload is invalid | Validation rejects it; the previous configuration stays in force | `reload_failures` on `GET :9090/config/routing` | unaffected |
| 29 | A request type has no routing rule at all | Pre-chain exit `guard.unconfigured_request_type`, reported to `routing_decision_total` rather than only logged | `routing_decision_total{routing_rule="guard.unconfigured_request_type"}` — alert on any non-zero rate | `503 NO_SOURCE_AVAILABLE` |
| 30 | Caller asks for `?consistency=strong` | The cache **read** is bypassed entirely; the loader runs and the result is still written back via `Manager.Store` for callers at weaker levels | `meta.cache.hit` always `false` | `200` |

---

## Appendix A — the response envelope as an audit record

Every success response carries the decision that produced it. This is what makes
the service operable by people who did not write it, and it is why the load tests
can assert *policy* correctness and not just transport correctness.

| Field | Answers |
|---|---|
| `meta.routingDecision` | Which source set answered: `OPERATIONAL`, `EXECUTION`, `BOTH`, `NONE` |
| `meta.routingRule` | *Which named rule decided that*, including `degrade.stale_cache` and `fallback.primary_failed` |
| `meta.sources` | The sources that actually contributed, `CACHE` included |
| `meta.freshness` | `state`, `ageSeconds`, `ttlSeconds`, `observedAt`, `evaluatedAt`, `source`, `skewCorrected`, `version`. `ageSeconds` and `ttlSeconds` come from a custom `MarshalJSON` on `domain.Freshness` — the underlying `time.Duration` fields would otherwise serialise as opaque nanosecond integers — and are **always** emitted, so `state: UNKNOWN` with `ageSeconds: 0` means "no age could be established" |
| `meta.degraded` | The answer is whole but older or from a fallback than intended |
| `meta.partial` | Some requested field group is absent; the response is `206` |
| `meta.cache` | `hit`, `layer` (`L1`/`L2`/`NONE`), `ageMs` |
| `meta.provenance` | Per-field, which source won |
| `meta.warnings` | One entry per thing the caller should know, each with a code and a source |
| `meta.elapsedMs` | Server-side wall time for the request |

Response headers carry the same three facts for anyone reading an access log
without a body parser: `X-BFF-Freshness`, `X-BFF-Source` and, when degraded,
`X-BFF-Degraded: true`.

## Appendix B — spans

These are the spans the service actually emits, read from the `StartSpan` calls
in `internal/**`:

| Span | Emitted by | Notes |
|---|---|---|
| the route pattern, e.g. `GET /api/v1/resources/{resourceId}/status` | `otelhttp` in `api.Server.publicRoutes`, via `WithSpanNameFormatter` | The server span. Named by route *pattern*, never by path, so trace cardinality stays bounded the same way metric attributes do. An unmatched request is named `<METHOD> unmatched` |
| `bff.usecase.resource` | `application.Service.resourceView` | Attributes: `request_type`, `view` (`full`/`status`/`configuration`/`details`) |
| `bff.usecase.execution` | `application.Service.executionView` | Attribute: `request_type` |
| `bff.route` | `application.Service.route` | Attributes: `routing_target`, `routing_rule`, `freshness` |
| `bff.aggregate` | `application.Service.fetch` | Attributes: `routing_target`, `routing_rule`. Wraps the fan-out |
| `bff.resolve_in_flight` | `application.Service.resolveInFlight` | Present **only** when the operational record declared an `in_flight_execution_ref`. Attribute: `execution_id` |

There is deliberately no span per source call, per mapper or per precedence
decision: those are recorded as metrics (`operational_source_latency`,
`execution_source_latency`, `precedence_conflict_total`) rather than as spans, so
a high-rate read path does not pay for span construction it will not be read
from.
