# TTL-Aware BFF — Data Contracts

Side-by-side definition of the three schemas the service reconciles, the complete
field-by-field mapping tables implemented by `internal/mapper`, enum mapping
tables, the declared drop list, schema versioning rules, and the shared-identity
keys used to correlate records across two sources that share no primary key
implementation.

Requirement ids from `spec/requirements.md`. Proto is
`api/proto/operational/v1/operational.proto`.

---

## 1. Why three schemas

| | ODS (operational) | EDS (execution) | Canonical (`internal/domain`) |
|---|---|---|---|
| Protocol | gRPC / protobuf | REST / JSON | Go structs, no wire form |
| Shape | resource-centric, current state | workflow-centric, event history |resource + execution facade |
| Vocabulary | `state`, `customer_ref`, `substate` | `status`, `customerId`, `phase` | `Status`, `CustomerID`, `LifecycleState` |
| Versioning | proto field numbers + `schema_version` string | JSON + `schemaVersion` field / `X-Schema-Version` header | Go type + cache `schemaVersion` |
| Time model | `google.protobuf.Timestamp`, source clock | RFC 3339 strings, source clock | `time.Time` UTC |
| Owner | operations platform team | workflow platform team | this service |

The vocabularies differ **on purpose** (stated in the proto header comment). The
canonical model is the only place the two are reconciled, and `internal/domain`
imports neither (REQ-MAP-001).

---

## 2. ODS schema (protobuf)

### 2.1 Service

| RPC | Request | Response | Used for |
|---|---|---|---|
| `GetResourceFreshness` | `GetResourceFreshnessRequest{context, resource_id}` | `GetResourceFreshnessResponse{resource_id, found, freshness}` | TTL decision before fetch (REQ-DS-002, REQ-RT-003) |
| `GetResource` | `GetResourceRequest{context, resource_id, field_mask[]}` | `GetResourceResponse{resource}` | `resource_read`, `resource_configuration`, `resource_details` |
| `GetResourceState` | `GetResourceStateRequest{context, resource_id}` | `GetResourceStateResponse{resource_id, state, substate, freshness, in_flight_execution_ref}` | `resource_status` (narrow read) |
| `BatchGetResources` | `BatchGetResourcesRequest{context, resource_ids[]}` | `BatchGetResourcesResponse{resources[], missing_resource_ids[]}` | fan-in optimisation |
| `Health` | `HealthRequest{}` | `HealthResponse{state, detail}` | background health poller (REQ-DS-006) |

### 2.2 `OperationalResource`

| # | Field | Proto type | Semantics |
|---|---|---|---|
| 1 | `resource_id` | `string` | identity, tenant-scoped |
| 2 | `customer_ref` | `string` | customer identity in ODS vocabulary |
| 3 | `tenant_id` | `string` | isolation key |
| 4 | `resource_type` | `string` | free-form type discriminator |
| 5 | `state` | `ResourceState` | source-native lifecycle enum |
| 6 | `substate` | `string` | free-form refinement of `state` |
| 7 | `ownership` | `OwnershipRecord` | owner id/type/email/cost centre |
| 8 | `configuration` | `map<string,string>` | effective configuration |
| 9 | `metrics` | `repeated MetricSample` | name/value/unit/sampled_at |
| 10 | `topology` | `TopologyRecord` | region/zone/cluster + refs |
| 11 | `operational_metadata` | `map<string,string>` | source-internal bookkeeping |
| 12 | `labels` | `map<string,string>` | user labels |
| 13 | `freshness` | `FreshnessEnvelope` | last_updated / server_time / refresh_source / version |
| 14 | `in_flight_execution_ref` | `string` | non-empty ⇒ an execution is mutating this resource (REQ-PREC-003) |
| 15 | `schema_version` | `string` | source contract version (REQ-EDGE-017) |

### 2.3 `FreshnessEnvelope`

| # | Field | Type | Role |
|---|---|---|---|
| 1 | `last_updated` | `Timestamp` | when the source last refreshed the record |
| 2 | `server_time` | `Timestamp` | response-generation time **in the source's clock**; enables single-domain age arithmetic (REQ-TTL-004) |
| 3 | `refresh_source` | `string` | `poller` \| `event-stream` \| `reconciler` — provenance of the refresh |
| 4 | `version` | `uint64` | monotonic record version, used for conflict detection |

### 2.4 Nested records

```
OwnershipRecord { owner_id, owner_type("team"|"user"|"service"), owner_email, cost_centre }
MetricSample    { name, value(double), unit, sampled_at(Timestamp) }
TopologyRecord  { region, zone, cluster, upstream_refs[], downstream_refs[] }
```

---

## 3. EDS schema (JSON)

### 3.1 Endpoints

| Method | Path | Returns |
|---|---|---|
| GET | `/eds/v1/executions?resourceId=&tenantId=&limit=` | `ExecutionListResponse` |
| GET | `/eds/v1/executions/{executionId}` | `ExecutionRecord` |
| GET | `/eds/v1/resources/{resourceId}/latest-execution` | `ExecutionRecord` or `404` |
| GET | `/eds/v1/health` | `HealthResponse` |

### 3.2 `ExecutionRecord`

```jsonc
{
  "executionId": "exec-8f21c4",
  "resourceId": "res-1042",
  "customerId": "cust-77",
  "tenantId": "acme",
  "workflowName": "resource.reconfigure",
  "workflowVersion": "3.1.0",
  "status": "RUNNING",                       // source vocabulary, see §6.2
  "phase": "APPLYING",                       // free-form refinement
  "startedAt": "2026-09-03T11:58:12.004Z",
  "updatedAt": "2026-09-03T12:00:41.771Z",
  "completedAt": null,
  "durationMs": 149767,
  "initiatedBy": { "principal": "svc-orchestrator", "kind": "service" },
  "steps": [
    { "stepId": "s1", "name": "validate",  "status": "COMPLETED",
      "startedAt": "...", "completedAt": "...", "attempt": 1,
      "output": { "checks": "12" } },
    { "stepId": "s2", "name": "apply",     "status": "RUNNING",
      "startedAt": "...", "completedAt": null, "attempt": 2, "output": null }
  ],
  "actions": [
    { "actionId": "a1", "type": "SCALE", "target": "res-1042",
      "requestedAt": "...", "status": "COMPLETED" }
  ],
  "result": null,                            // populated only on terminal status
  "error": null,                             // populated only on FAILED/TIMED_OUT
  "audit": [
    { "entryId": "au1", "at": "...", "actor": "svc-orchestrator",
      "action": "EXECUTION_STARTED", "detail": "..." }
  ],
  "schemaVersion": "2.4.0",
  "observedAt": "2026-09-03T12:00:41.900Z",  // EDS response-generation time
  "internalTraceId": "eds-7c1f…"             // dropped, see §7
}
```

`result` when present:
```jsonc
{ "outcome": "SUCCESS", "summary": "...", "artifacts": [ {"name":"plan","uri":"..."} ],
  "metrics": { "changedFields": 4 } }
```

`error` when present:
```jsonc
{ "code": "STEP_FAILED", "message": "...", "stepId": "s2", "retryable": true,
  "occurredAt": "..." }
```

### 3.3 `ExecutionListResponse`

```jsonc
{
  "executions": [ /* ExecutionRecord, newest first by startedAt */ ],
  "nextCursor": "eyJvIjoxMDB9",
  "totalKnown": 143,
  "schemaVersion": "2.4.0",
  "observedAt": "2026-09-03T12:00:41.900Z"
}
```

### 3.4 `HealthResponse`

```jsonc
{ "state": "SERVING", "detail": "", "schemaVersion": "2.4.0" }
```
`state` ∈ `SERVING | DEGRADED | NOT_SERVING`.

---

## 4. Canonical model (`internal/domain`)

Types fixed by the contract: `Resource`, `ResourceStatus`, `Owner`, `Metric`,
`Topology`, `Execution`, `ExecutionStatus`, `WorkflowStep`, `Action`,
`ExecutionResult`, `ExecutionError`, `AuditEntry`, `ResourceDetails`,
`Freshness`, `FreshnessState`, `SourceKind`, `Provenance`, `ResponseMeta`.

```go
type Resource struct {
    ResourceID     string
    CustomerID     string
    TenantID       string
    ResourceType   string
    Status         ResourceStatus
    LifecycleState string             // from ODS substate
    Owner          *Owner             // nil when unsupplied (REQ-PREC-005)
    Configuration  map[string]string  // nil when unsupplied; never empty-map-as-absent
    Metrics        []Metric
    Topology       *Topology
    Labels         map[string]string
    Freshness      Freshness
}

type Execution struct {
    ExecutionID     string
    ResourceID      string
    CustomerID      string
    TenantID        string
    WorkflowName    string
    WorkflowVersion string
    Status          ExecutionStatus
    Phase           string
    StartedAt       time.Time
    UpdatedAt       time.Time
    CompletedAt     time.Time          // zero when not terminal
    Duration        time.Duration
    InitiatedBy     string
    Steps           []WorkflowStep
    Actions         []Action
    Result          *ExecutionResult
    Error           *ExecutionError
    Audit           []AuditEntry
    Freshness       Freshness
}

type ResourceDetails struct {
    Resource         Resource
    LatestExecution  *Execution
    ExecutionHistory []Execution
    LastOperation    *Action
}

type Freshness struct {
    State      FreshnessState  // FRESH | STALE | UNKNOWN
    AgeSeconds *float64        // nil when UNKNOWN (REQ-API-005)
    TTLSeconds float64
    ObservedAt time.Time
    Source     SourceKind
    Version    uint64          // from ODS FreshnessEnvelope.version, 0 when absent
}
```

Enumerations (contract §4):

```
SourceKind      ∈ OPERATIONAL | EXECUTION | CACHE | NONE
FreshnessState  ∈ FRESH | STALE | UNKNOWN
ResourceStatus  ∈ PENDING | ACTIVE | SUSPENDED | DEGRADED | TERMINATING | TERMINATED | ERROR | UNKNOWN
ExecutionStatus ∈ QUEUED | RUNNING | COMPLETED | FAILED | CANCELLED | TIMED_OUT | UNKNOWN
```

**Pointer vs zero-value discipline.** Optional structures (`Owner`, `Topology`,
`Result`, `Error`, `AgeSeconds`) are pointers so that "absent" and "present but
zero" are distinguishable. This is what makes REQ-PREC-005 ("empty is not a
value") implementable and REQ-EDGE-006/007 (partial payloads) correct.

---

## 5. OperationalMapper — field-by-field

`internal/mapper/operational.go`. Signature (REQ-MAP-007):
`MapResource(ctx MapContext, in *opsv1.OperationalResource) (domain.Resource, []Warning, error)`.

| ODS field | Canonical field | Transform | Absent/zero handling | Req |
|---|---|---|---|---|
| `resource_id` | `Resource.ResourceID` | verbatim | **mandatory** — empty ⇒ `UPSTREAM_INVALID_RESPONSE` | REQ-MAP-008 |
| `customer_ref` | `Resource.CustomerID` | rename only | empty ⇒ empty string, warn `PARTIAL_DATA` | REQ-MAP-005 |
| `tenant_id` | `Resource.TenantID` | verbatim + assertion | **mandatory**; `≠ ctx.Tenant` ⇒ `TENANT_MISMATCH` | REQ-SEC-006 |
| `resource_type` | `Resource.ResourceType` | verbatim | empty ⇒ empty string | |
| `state` | `Resource.Status` | enum table §6.1 | `UNSPECIFIED` ⇒ `UNKNOWN` | REQ-MAP-003 |
| `substate` | `Resource.SubState` | rename only | empty ⇒ empty string | REQ-MAP-005 |
| `ownership` | `Resource.Owner` | struct map, see below | absent/all-empty ⇒ `nil` (not `&Owner{}`) | REQ-PREC-005 |
| `ownership.owner_id` | `Owner.ID` | verbatim | | |
| `ownership.owner_type` | `Owner.Type` | lowercase, validate ∈ `team\|user\|service`; unknown ⇒ verbatim + `SCHEMA_VERSION_MISMATCH` | | REQ-MAP-002 |
| `ownership.owner_email` | `Owner.Email` | verbatim; redacted on output per RBAC | | REQ-SEC-011 |
| `ownership.cost_centre` | — | **dropped** | | REQ-MAP-006 |
| `configuration` | `Resource.Configuration` | copy map; keys matching secret patterns redacted at output | empty map ⇒ `nil` | REQ-SEC-011, REQ-PREC-005 |
| `metrics[]` | `Resource.Metrics[]` | element map, see below | empty ⇒ `nil` slice | |
| `metrics[].name` | `Metric.Name` | verbatim | empty element ⇒ element skipped + warn | |
| `metrics[].value` | `Metric.Value` | `float64` verbatim | `NaN`/`±Inf` ⇒ `UPSTREAM_INVALID_RESPONSE` | REQ-MAP-008 |
| `metrics[].unit` | `Metric.Unit` | verbatim, **no unit conversion** | empty ⇒ empty string | REQ-MAP-009 |
| `metrics[].sampled_at` | `Metric.SampledAt` | `Timestamp` → UTC `time.Time` | zero ⇒ zero `time.Time` (absent) | REQ-MAP-010 |
| `topology` | `Resource.Topology` | struct map | all-empty ⇒ `nil` | REQ-PREC-005 |
| `topology.region` | `Topology.Region` | verbatim | | |
| `topology.zone` | `Topology.Zone` | verbatim | | |
| `topology.cluster` | `Topology.Cluster` | verbatim | | |
| `topology.upstream_refs` | `Topology.Upstream` | rename; order preserved | empty ⇒ `nil` | REQ-MAP-005 |
| `topology.downstream_refs` | `Topology.Downstream` | rename; order preserved | empty ⇒ `nil` | REQ-MAP-005 |
| `operational_metadata` | — | **dropped** | | REQ-MAP-006 |
| `labels` | `Resource.Labels` | copy map | empty ⇒ `nil` | |
| `freshness.last_updated` | `Freshness.ObservedAt` | `Timestamp` → UTC | zero ⇒ absent ⇒ `FreshnessState = UNKNOWN` | REQ-MAP-010, REQ-TTL-002 |
| `freshness.server_time` | (age arithmetic input) | not surfaced directly | absent ⇒ two-clock fallback path | REQ-TTL-004 |
| `freshness.refresh_source` | — | **dropped** (logged at debug) | | REQ-MAP-006 |
| `freshness.version` | `Freshness.Version` | verbatim | `0` ⇒ version-based conflict detection disabled | |
| — | `Freshness.State` | computed by `internal/freshness`, not the mapper | | REQ-TTL-002 |
| — | `Freshness.Source` | set to `OPERATIONAL` | | |
| `in_flight_execution_ref` | `MapResult.InFlightExecutionRef` | not a `Resource` field; passed to `SourcePrecedencePolicy` | empty ⇒ no override signal | REQ-PREC-003 |
| `schema_version` | `MapResult.SchemaVersion` | parsed semver; gated per §8 | empty ⇒ treated as `1.0.0` + `SCHEMA_VERSION_MISMATCH` | REQ-EDGE-017 |

### 5.1 `GetResourceStateResponse` (narrow read)

| # | ODS field | Canonical | Notes |
|---|---|---|---|
| 1 | `resource_id` | `Resource.ResourceID` | mandatory |
| 2 | `state` | `Resource.Status` | table §6.1 |
| 3 | `substate` | `Resource.SubState` | |
| 4 | `freshness` | `Resource.Freshness` | as above |
| 5 | `in_flight_execution_ref` | `Resource.InFlightExecutionID` | carried on the narrow read as well as the full one (REQ-PREC-003) |

All other `Resource` fields are left absent — this is a legitimate partial by
construction, and MUST NOT warn `PARTIAL_DATA` because the request type
(`resource_status`) only requires `status` and `subState` (REQ-EDGE-006).

**Why field 5 is on the narrow read too.** Without it the BFF could not tell,
from a cheap status read, that a workflow is mutating the resource — and the
"a running execution may override operational state" precedence rule could only
ever fire on the expensive both-source endpoints, leaving `/status` and
`/details` disagreeing mid-workflow. It is also what
`routing.defaults.resolve_in_flight_execution` keys off: the operational-only
read makes its one extra execution-source call precisely when this field is
non-empty, so the common case pays nothing.

### 5.2 `GetResourceFreshnessResponse` (probe)

| ODS field | Consumed as | Notes |
|---|---|---|
| `resource_id` | echo check | mismatch ⇒ `UPSTREAM_INVALID_RESPONSE` |
| `found` | not-found signal | `false` ⇒ `404 NOT_FOUND`, negatively cached (REQ-CACHE-005) |
| `freshness` | probe observation | memoized as the **observation**, never as a verdict (REQ-TTL-006) |

---

## 6. Enum mapping tables

### 6.1 `ResourceState` → `domain.ResourceStatus` (REQ-MAP-003)

| ODS `ResourceState` | # | Canonical `ResourceStatus` | Note |
|---|---|---|---|
| `RESOURCE_STATE_UNSPECIFIED` | 0 | `UNKNOWN` | proto3 default; also the wire value for a field the source did not set |
| `RESOURCE_STATE_PROVISIONING` | 1 | `PENDING` | **vocabulary difference is intentional** |
| `RESOURCE_STATE_ACTIVE` | 2 | `ACTIVE` | |
| `RESOURCE_STATE_SUSPENDED` | 3 | `SUSPENDED` | |
| `RESOURCE_STATE_DEGRADED` | 4 | `DEGRADED` | |
| `RESOURCE_STATE_TERMINATING` | 5 | `TERMINATING` | |
| `RESOURCE_STATE_TERMINATED` | 6 | `TERMINATED` | |
| `RESOURCE_STATE_ERROR` | 7 | `ERROR` | |
| *any unrecognized number* | ≥8 | `UNKNOWN` + warning `SCHEMA_VERSION_MISMATCH` | MUST NOT guess a neighbour (REQ-MAP-002) |

### 6.2 EDS `status` string → `domain.ExecutionStatus` (REQ-MAP-004)

Comparison is case-insensitive after trimming.

| EDS value(s) | Canonical `ExecutionStatus` |
|---|---|
| `queued`, `pending`, `scheduled` | `QUEUED` |
| `running`, `in_progress`, `executing` | `RUNNING` |
| `completed`, `succeeded`, `success` | `COMPLETED` |
| `failed`, `error` | `FAILED` |
| `cancelled`, `canceled`, `aborted` | `CANCELLED` |
| `timed_out`, `timeout` | `TIMED_OUT` |
| *anything else, or empty* | `UNKNOWN` + warning `SCHEMA_VERSION_MISMATCH` |

Multiple source spellings map to one canonical value because the EDS accumulated
synonyms across workflow-engine versions. The canonical set is closed; new source
spellings surface as `UNKNOWN` with a drift warning rather than silently
appearing in the API.

### 6.3 EDS step/action `status` → `domain.ExecutionStatus`

Same table as §6.2, applied to `steps[].status` and `actions[].status`.

### 6.4 ODS `HealthState` → `domain.SourceHealth`

| ODS | Canonical | Router treats as |
|---|---|---|
| `HEALTH_STATE_UNSPECIFIED` | `UNKNOWN` | unavailable |
| `HEALTH_STATE_SERVING` | `SERVING` | available |
| `HEALTH_STATE_DEGRADED` | `DEGRADED` | available |
| `HEALTH_STATE_NOT_SERVING` | `NOT_SERVING` | unavailable |
| *(BFF-internal)* | `CIRCUIT_OPEN` | unavailable |

### 6.5 EDS `state` string → `domain.SourceHealth`

`SERVING → SERVING`, `DEGRADED → DEGRADED`, `NOT_SERVING → NOT_SERVING`,
anything else → `UNKNOWN` (treated as unavailable).

### 6.6 `initiatedBy.kind` → `Owner.Type` vocabulary alignment

| EDS `kind` | Canonical | Note |
|---|---|---|
| `service` | `service` | |
| `user` | `user` | |
| `team` | `team` | |
| *other* | verbatim + `SCHEMA_VERSION_MISMATCH` | shared vocabulary with ODS `owner_type` |

---

## 7. ExecutionMapper — field-by-field

`internal/mapper/execution.go`. Signature:
`MapExecution(ctx MapContext, in ExecutionRecordDTO) (domain.Execution, []Warning, error)`.

| EDS field | Canonical field | Transform | Absent/zero handling | Req |
|---|---|---|---|---|
| `executionId` | `Execution.ExecutionID` | verbatim | **mandatory** ⇒ else `UPSTREAM_INVALID_RESPONSE` | REQ-MAP-008 |
| `resourceId` | `Execution.ResourceID` | verbatim | **mandatory** | REQ-MAP-008 |
| `customerId` | `Execution.CustomerID` | verbatim | empty ⇒ empty string | |
| `tenantId` | `Execution.TenantID` | verbatim + assertion | **mandatory**; mismatch ⇒ `TENANT_MISMATCH` | REQ-SEC-006 |
| `workflowName` | `Execution.WorkflowName` | verbatim | | |
| `workflowVersion` | `Execution.WorkflowVersion` | verbatim | | |
| `status` | `Execution.Status` | enum table §6.2 | empty/unknown ⇒ `UNKNOWN` + drift warning | REQ-MAP-004 |
| `phase` | `Execution.Phase` | verbatim | | |
| `startedAt` | `Execution.StartedAt` | RFC 3339 → UTC | unparseable ⇒ `UPSTREAM_INVALID_RESPONSE`; `null` ⇒ zero time | REQ-MAP-008/010 |
| `updatedAt` | `Execution.UpdatedAt` | RFC 3339 → UTC | `null` ⇒ zero time | |
| `completedAt` | `Execution.CompletedAt` | RFC 3339 → UTC | `null` on non-terminal status is **normal**, no warning | REQ-EDGE-007 |
| `durationMs` | `Execution.Duration` | `ms → time.Duration` | absent ⇒ derived from `completedAt − startedAt` when both present, else zero | |
| `initiatedBy.principal` | `Execution.InitiatedBy` | flatten to string | absent ⇒ empty | |
| `initiatedBy.kind` | (validation only) | table §6.6 | | |
| `steps[]` | `Execution.Steps[]` | element map | `null`/absent ⇒ `nil` slice, no warning | REQ-EDGE-007 |
| `steps[].stepId` | `WorkflowStep.StepID` | verbatim | empty ⇒ element skipped + warn | |
| `steps[].name` | `WorkflowStep.Name` | verbatim | | |
| `steps[].status` | `WorkflowStep.Status` | table §6.3 | | |
| `steps[].startedAt` / `completedAt` | `WorkflowStep.StartedAt` / `CompletedAt` | RFC 3339 → UTC | `null` ⇒ zero | |
| `steps[].attempt` | `WorkflowStep.Attempt` | `int` | absent ⇒ `1` (defaulted, no warning) | REQ-MAP-008 |
| `steps[].output` | `WorkflowStep.Output` | `map[string]string` copy | `null` ⇒ `nil` | |
| `actions[]` | `Execution.Actions[]` | element map | absent ⇒ `nil` | |
| `actions[].actionId` | `Action.ActionID` | verbatim | mandatory within element | |
| `actions[].type` | `Action.Type` | verbatim | | |
| `actions[].target` | `Action.Target` | verbatim | | |
| `actions[].requestedAt` | `Action.RequestedAt` | RFC 3339 → UTC | | |
| `actions[].status` | `Action.Status` | table §6.3 | | |
| `result` | `Execution.Result` | struct map | `null` ⇒ `nil`. `null` **with terminal status** ⇒ warn `PARTIAL_DATA` | REQ-EDGE-007 |
| `result.outcome` | `ExecutionResult.Outcome` | verbatim | | |
| `result.summary` | `ExecutionResult.Summary` | verbatim | | |
| `result.artifacts[]` | `ExecutionResult.Artifacts[]` | `{name, uri}` | | |
| `result.metrics` | `ExecutionResult.Metrics` | `map[string]float64` | `NaN`/`Inf` ⇒ invalid | REQ-MAP-008 |
| `error` | `Execution.Error` | struct map | `null` on non-`FAILED` is normal | REQ-EDGE-007 |
| `error.code` | `ExecutionError.Code` | verbatim (source vocabulary, **not** a BFF error code) | | |
| `error.message` | `ExecutionError.Message` | verbatim, truncated to 2 KiB | | |
| `error.stepId` | `ExecutionError.StepID` | verbatim | | |
| `error.retryable` | `ExecutionError.Retryable` | verbatim; **advisory only** — does not drive BFF retry (REQ-RES-003) | absent ⇒ `false` | |
| `error.occurredAt` | `ExecutionError.OccurredAt` | RFC 3339 → UTC | | |
| `audit[]` | `Execution.Audit[]` | element map | absent ⇒ `nil` | |
| `audit[].entryId` | `AuditEntry.EntryID` | verbatim | | |
| `audit[].at` | `AuditEntry.At` | RFC 3339 → UTC | | |
| `audit[].actor` | `AuditEntry.Actor` | verbatim | | |
| `audit[].action` | `AuditEntry.Action` | verbatim | | |
| `audit[].detail` | `AuditEntry.Detail` | verbatim, truncated to 2 KiB | | |
| `observedAt` | `Freshness.ObservedAt` | RFC 3339 → UTC | absent ⇒ falls back to `updatedAt`; both absent ⇒ `UNKNOWN` | REQ-TTL-002 |
| `schemaVersion` | `MapResult.SchemaVersion` | parsed semver; gated per §8 | absent ⇒ `X-Schema-Version` header, else `1.0.0` + drift | REQ-EDGE-017 |
| `internalTraceId` | — | **dropped** | | REQ-MAP-006 |
| — | `Freshness.Source` | set to `EXECUTION` | | |
| — | `Freshness.Version` | `0` — the EDS has no monotonic record version | | |

### 7.1 `ExecutionListResponse`

| EDS field | Canonical | Notes |
|---|---|---|
| `executions[]` | `[]domain.Execution` | ordered newest-first by `startedAt`, ties broken by `executionId` (REQ-API-012) |
| `nextCursor` | pagination cursor | opaque, re-emitted verbatim; validated ≤512 B base64url (REQ-SEC-009) |
| `totalKnown` | not surfaced in v1 | dropped (declared) |
| `observedAt` | `Freshness.ObservedAt` for the collection | |
| `schemaVersion` | version gate | §8 |

An `executions: []` with `200` is an **empty collection**, not a not-found: the
canonical response is `200` with `data.executions: []` and warning
no warning (REQ-EDGE-019). A `404` on `latest-execution` means the resource
has never executed and maps to `LatestExecution = nil`, not to a request-level
`404`.

### 7.2 Derived field: `lastOperation`

`ResourceDetails.LastOperation` is **derived**, not mapped: it is the most recent
`Action` (by `requestedAt`) belonging to the most recent terminal `Execution`.
When no terminal execution exists, it is `nil`. Derivation lives in
`internal/mapper/derive.go` and is covered by `TestMapper_DeriveLastOperation`.

---

## 8. Declared drop list (REQ-MAP-006)

A source field that is neither mapped above nor listed here fails
`TestMapper_CompletenessAgainstDropList`. Silent drops are prohibited.

| Source | Field | Reason for dropping |
|---|---|---|
| ODS | `OperationalResource.operational_metadata` | source-internal bookkeeping; no canonical meaning, unbounded key space |
| ODS | `OwnershipRecord.cost_centre` | financial attribution, out of BFF scope; would require its own RBAC treatment |
| ODS | `FreshnessEnvelope.refresh_source` | diagnostic; logged at debug, not surfaced and not traced |
| ODS | `GetResourceRequest.field_mask` | request-side only; the BFF sets it from `RequiredFields`, never echoes it |
| ODS | `BatchGetResourcesResponse.missing_resource_ids` | consumed for not-found accounting, not surfaced in the envelope |
| EDS | `internalTraceId` | EDS-internal correlation; the BFF propagates its own `correlationId` and W3C `traceparent` |
| EDS | `ExecutionListResponse.totalKnown` | unstable under concurrent writes; exposing it would imply a consistency guarantee the EDS does not make |
| EDS | `HealthResponse.detail` | free-form; logged, not surfaced |

### 8.1 Defaulted fields

| Source | Field | Default | Warning? |
|---|---|---|---|
| EDS | `steps[].attempt` | `1` | no |
| EDS | `error.retryable` | `false` | no |
| EDS | `durationMs` | derived from timestamps | no |
| EDS | `observedAt` | falls back to `updatedAt` | no |
| EDS | `schemaVersion` | header, then `1.0.0` | `SCHEMA_VERSION_MISMATCH` when both absent |
| ODS | `schema_version` | `1.0.0` | `SCHEMA_VERSION_MISMATCH` when absent |
| ODS | `metrics[].unit` | `""` | no |
| ODS | `freshness.version` | `0` (disables version conflict detection) | no |

---

## 9. Schema versioning and compatibility

### 9.1 Version carriers

| Schema | Carrier | Example |
|---|---|---|
| ODS | `OperationalResource.schema_version` (field 15) | `"1.7.0"` |
| EDS | body `schemaVersion`, else `X-Schema-Version` header | `"2.4.0"` |
| BFF public API | URL path `/api/v1` + envelope shape | `v1` |
| Cache entry | `schemaVersion` inside the stored entry | `"bff-1"` |

### 9.2 Compatibility rules (REQ-EDGE-017)

The BFF declares, per source, a **supported major** and a **maximum known minor**.

| Source version vs BFF support | Classification | Behaviour |
|---|---|---|
| same major, minor ≤ known | compatible | normal processing |
| same major, minor > known | **forward drift, tolerated** | unknown JSON fields ignored; unknown proto fields ignored; unknown enum members → `UNKNOWN`; warning `SCHEMA_VERSION_MISMATCH`; response is `200` |
| same major, minor < known | backward, tolerated | fields the BFF expects may be absent → optional-field rules apply |
| different major | **incompatible** | `SCHEMA_VERSION_MISMATCH` (terminal, not retryable, REQ-RES-003). The source is **not** marked unavailable and its breaker is **not** tripped — it is healthy, and only its contract is unintelligible. `errs.SourceUnusable` is true for this code, so the *call-time* fallback (`fallback.primary_failed`) selects the other source where one is configured; where none is, the result is `502`. |
| absent | unknown | assumed `1.0.0`, drift warning |

**Rationale for tolerating minor drift.** The two sources deploy on their own
cadence. A BFF that hard-fails on any unrecognised field would turn every
upstream feature release into a BFF outage. A BFF that silently guesses at
unknown enum values would turn it into a correctness incident. Ignore-unknown +
`UNKNOWN` + warning is the only combination that is both available and honest.

**Rationale for failing major drift.** A major bump means field *semantics*
changed, not just field presence. Continuing to map by field name would produce
plausible-looking wrong data — the worst possible failure mode. Marking the
source unavailable converts a silent correctness failure into an explicit
availability failure that the routing chain already knows how to handle.

### 9.3 Proto evolution rules (ODS)

| Change | Allowed | Requires |
|---|---|---|
| add a field with a new number | yes | minor bump; BFF ignores it |
| add an enum member | yes | minor bump; BFF maps to `UNKNOWN` + drift warning |
| rename a field (same number) | yes on the wire | minor bump; BFF is number-driven via generated code |
| change a field's type | **no** | major bump |
| reuse a field number | **no** | prohibited outright; `reserved` required |
| remove a field | discouraged | major bump if the BFF maps it (fields 1,3,5,13 are load-bearing) |
| change enum member semantics | **no** | major bump |

### 9.4 JSON evolution rules (EDS)

| Change | Allowed | Requires |
|---|---|---|
| add a property | yes | minor bump; BFF ignores unknown properties |
| add a `status` spelling | yes | minor bump; add a row to §6.2 in the same release or accept `UNKNOWN` |
| make an optional property absent | yes | minor bump |
| make a mandatory property absent | **no** | major bump (`executionId`, `resourceId`, `tenantId`, `status`) |
| change a property's type | **no** | major bump |
| change timestamp format from RFC 3339 | **no** | major bump |

### 9.5 BFF public API compatibility

Additive only within `/api/v1`: new `meta` fields, new warning codes, new
optional `data` fields. Removing a field, changing a type, changing a status-code
mapping or removing an error code requires `/api/v2`. New error codes are treated
as additive because clients are required to handle unknown codes by falling back
to the HTTP status (documented in `spec/error-model.md`).

### 9.6 Cache entry versioning (REQ-CACHE-007)

Entries carry `schemaVersion`. A mismatch is treated as a miss and the entry is
evicted; it is never decoded into a struct of a different shape. Bumping the
canonical model's serialized form is therefore safe during a rolling deploy: old
and new replicas simply miss each other's entries rather than corrupting them.

---

## 10. Shared identity and record correlation

The two sources share **no primary key implementation**. Correlation is by four
agreed identity keys.

| Key | ODS carrier | EDS carrier | Canonical | Role |
|---|---|---|---|---|
| `tenantId` | `RequestContext.tenant_id`, `OperationalResource.tenant_id` | query param `tenantId`, body `tenantId` | `TenantID` | **isolation boundary** — mandatory everywhere |
| `customerId` | `OperationalResource.customer_ref` | `customerId` | `CustomerID` | business grouping; informational, never a join key |
| `resourceId` | `resource_id` (all RPCs) | `resourceId` (query + body) | `ResourceID` | **the join key** between the two sources |
| `executionId` | `in_flight_execution_ref` (weak reference) | `executionId` | `ExecutionID` | identifies a workflow run; ODS holds at most a reference |

### 10.1 Correlation algorithm

```
correlate(tenant, resourceId):
    // 1. Isolation gate — before anything else.
    assert every fetched record has record.tenantId == tenant     (REQ-SEC-006)
        else → TENANT_MISMATCH + audit event

    // 2. Join. resourceId is the only key both sources agree on.
    ops  := ODS.GetResource(tenant, resourceId)          // 0 or 1 record
    exec := EDS.LatestExecution(tenant, resourceId)      // 0 or 1 record
    hist := EDS.ListExecutions(tenant, resourceId, page) // 0..n records

    // 3. Cross-check the weak reference.
    if ops.in_flight_execution_ref != "":
        if exec != nil && exec.executionId == ops.in_flight_execution_ref:
            runningSignal = CONFIRMED       // both sources agree
        else:
            runningSignal = ODS_ONLY        // ODS says running, EDS has not caught up
            warn CONFLICT_RESOLVED only if the resolved values actually differ

    // 4. customerId is NOT used to join. It is compared for observability only.
    if ops.customerId != "" && exec != nil && exec.customerId != "" &&
       ops.customerId != exec.customerId:
        warn CONFLICT_RESOLVED{field_group: "identity"}   // data-quality signal
```

### 10.2 Why `resourceId` and not `customerId`

`customerId` is a *grouping* attribute maintained independently in both systems
and is observed to drift (renames, merges, backfills). Joining on it would
produce cross-resource contamination. `resourceId` is the identifier the caller
supplied in the URL; using it as the sole join key means the join can never widen
beyond what the caller asked for. A `customerId` disagreement is therefore
surfaced as a data-quality warning, never acted upon.

### 10.3 Why `executionId` is a weak reference

`in_flight_execution_ref` is the ODS's *belief* about an in-flight execution. It
can be stale in both directions: set after the execution finished, or unset while
one is starting. Consequently:

- It is sufficient to **trigger the lookup** that may in turn enable the
  running-execution precedence override (REQ-PREC-003) — but it is **not**
  sufficient to enable the override itself. `Merger.Merge` sets
  `ExecutionInProgress` only when an execution *candidate* exists whose status is
  actually `InProgress`. A marker left behind by a workflow that has since
  finished is reported (on the precedence context, and as
  `data.inFlightExecutionId`) but confers no authority, so a completed run's
  *predicted* status can never outrank the operational source's *observed* one —
  and `/details` cannot disagree with `/status` as a result of a stale marker,
  which is the same invariant the in-flight resolution exists to protect.
- On a **both-source** read it is **not** used to fetch by: the BFF resolves the
  latest execution via `GET /eds/v1/resources/{resourceId}/latest-execution`,
  keyed on `resourceId`, rather than
  `GET /eds/v1/executions/{in_flight_execution_ref}`. A dangling reference would
  otherwise produce a `404` on a healthy resource.
- On an **operational-only** read there is no latest-execution call to piggyback
  on, so `Service.resolveInFlight` does fetch the reference directly, under
  `routing.defaults.in_flight_lookup_timeout` (300ms). That call is best-effort
  by construction: any error — including the `404` a dangling reference produces
  — is logged at debug and the operational answer stands unchanged. Only an
  execution the EDS still reports as in progress may override; one that finished
  since the operational record was written has no claim on current state.

### 10.4 Freshness correlation

The two records carry independent observation times from independent clocks.
They are **not** compared to decide precedence (REQ-PREC-006, REQ-EDGE-009).

Nor are they combined. `Service.reportedFreshness` publishes the **operational**
observation whenever the operational source contributed, and `UNKNOWN` with
`source: EXECUTION` otherwise: only the ODS makes a freshness guarantee, and
mixing in an EDS timestamp would manufacture a verdict neither source offered.

### 10.5 Identity validation rules

| Check | Applied to | Violation |
|---|---|---|
| `tenantId == authenticated tenant` | every mapped record | `TENANT_MISMATCH` + audit (REQ-SEC-006) |
| `resourceId == requested resourceId` | ODS resource, EDS executions | `UPSTREAM_INVALID_RESPONSE` |
| `resourceId` matches `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` | inbound path param | `400 INVALID_REQUEST` (REQ-SEC-009) |
| `executionId` present and non-empty | every EDS execution record | `UPSTREAM_INVALID_RESPONSE` |
| `executionId` unique within a list | `ExecutionListResponse` | duplicate ⇒ first kept, warn `PARTIAL_DATA` |

---

## 11. Mapping invariants (test obligations)

| Invariant | Test | Req |
|---|---|---|
| `internal/domain` imports nothing from this module or any transport package | `TestDomain_NoOutboundImports` | REQ-MAP-001 |
| Every ODS enum member has a row in §6.1 | `TestMapper_EnumTotality_Operational` | REQ-MAP-002 |
| Every EDS status spelling in §6.2 maps, unknowns → `UNKNOWN` + drift | `TestMapper_EnumTotality_Execution`, `TestMapper_UnknownEnumWarns` | REQ-MAP-002/004 |
| Every source field is mapped or on the drop list | `TestMapper_CompletenessAgainstDropList` | REQ-MAP-006 |
| Renames match §5/§7 exactly | `TestMapper_FieldRenameTable` | REQ-MAP-005 |
| Mappers perform no I/O and read no global clock | `TestMapper_Purity` | REQ-MAP-007 |
| Mandatory-field absence ⇒ `UPSTREAM_INVALID_RESPONSE` | `TestMapper_RejectsMalformed` | REQ-MAP-008 |
| Optional-field absence ⇒ defaulted, documented | `TestMapper_DefaultsOptional` | REQ-MAP-008 |
| Units are never converted | `TestMapper_UnitsPreserved` | REQ-MAP-009 |
| Zero timestamps are absent, not epoch | `TestMapper_ZeroTimestampIsAbsent` | REQ-MAP-010 |
| Foreign-tenant records are rejected | `TestTenant_ResponseTenantAsserted` | REQ-SEC-006 |
| `lastOperation` derivation | `TestMapper_DeriveLastOperation` | — |
| Golden fixtures for both sources round-trip to stable canonical JSON | `TestMapper_GoldenFixtures` | REQ-API-012 |
