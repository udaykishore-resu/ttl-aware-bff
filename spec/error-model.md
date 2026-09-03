# TTL-Aware BFF — Error Model

The typed error taxonomy in `pkg/errs`, its three-valued classification, the
adapter → code → HTTP mapping, the degradation decision table, and the retry
eligibility rules that implement "never retry blindly".

Error codes are frozen by `docs/DESIGN-CONTRACT.md` §3. Requirements from
`spec/requirements.md`.

---

## 1. Design position

Three rules govern everything below.

1. **Errors are values with declared behaviour, not strings.** Every failure
   crossing a port boundary is a `*errs.Error` carrying a code and a
   classification. No `status.Status`, no `*http.Response`, no wrapped
   `fmt.Errorf` chain is interpreted by string matching (REQ-DS-007).
2. **Classification decides, not the call site.** Whether to retry, whether to
   degrade, and what HTTP status to emit are all functions of the error value.
   A call site that inspects an error to decide "this one looks transient" is a
   defect (REQ-RES-003).
3. **A degraded answer is a success.** The error model's job is to distinguish
   *no answer* from *a worse answer*. `200`-degraded, `206`-partial and `503`
   are three different outcomes, and collapsing them is the most common failure
   of BFF error handling.

---

## 2. `pkg/errs` taxonomy

```go
package errs

type Code string

const (
    CodeInvalidRequest         Code = "INVALID_REQUEST"
    CodeUnauthenticated        Code = "UNAUTHENTICATED"
    CodeForbidden              Code = "FORBIDDEN"
    CodeTenantMismatch         Code = "TENANT_MISMATCH"
    CodeNotFound               Code = "NOT_FOUND"
    CodeRateLimited            Code = "RATE_LIMITED"
    CodeRequestTooLarge        Code = "REQUEST_TOO_LARGE"
    CodeUpstreamTimeout        Code = "UPSTREAM_TIMEOUT"
    CodeUpstreamUnavailable    Code = "UPSTREAM_UNAVAILABLE"
    CodeUpstreamInvalidResponse Code = "UPSTREAM_INVALID_RESPONSE"
    CodeSchemaVersionMismatch  Code = "SCHEMA_VERSION_MISMATCH"
    CodeNoSourceAvailable      Code = "NO_SOURCE_AVAILABLE"
    CodeInternal               Code = "INTERNAL"
)

type Error struct {
    Code        Code
    Message     string          // internal detail; logged, never returned verbatim
    Public      string          // client-safe detail (REQ-SEC-013)
    Source      domain.SourceKind
    SourceState SourceState     // CIRCUIT_OPEN, TIMEOUT, INVALID_RESPONSE, ...
    Retryable   bool
    Terminal    bool
    Degradable  bool
    RetryAfter  time.Duration   // honoured upstream hint, zero when none
    Cause       error
}

func (e *Error) Error() string { ... }
func (e *Error) Unwrap() error { return e.Cause }

func Is(err error, c Code) bool
func From(err error) *Error      // never nil; wraps unknown errors as CodeInternal
```

`Public` and `Message` are separate fields so that leaking internals requires a
deliberate act rather than an omission (REQ-SEC-013). The response encoder reads
`Public`; the logger reads `Message`.

### 2.1 The closed code set

`Code` is closed (REQ-API-009). `TestErrorCodes_ClosedSet` asserts that the
constant set, the OpenAPI `ErrorCode` enum and the contract's list agree
exactly. `type` is derived mechanically as
`https://errors.bff.internal/<kebab-case(code)>`, asserted by
`TestErrorType_URIDerivation`.

---

## 3. Three-valued classification

Every error carries three independent booleans (REQ-RES-004). They answer three
different questions and are not redundant.

| Flag | Question | Consumer |
|---|---|---|
| `Retryable` | *Would issuing this same call again plausibly succeed?* | `internal/resilience` retry |
| `Terminal` | *Is this settled — will it fail identically forever?* | breaker accounting, negative caching, client `retryable` hint |
| `Degradable` | *May this failure be answered with stale, cached or partial data?* | `internal/application` degradation ladder |

There is a fourth question, and it is the one that decides the call-time
fallback:

| Question | Asked as | Consumed by |
|---|---|---|
| `SourceUnusable` | *Can this source not serve the request, while a different one might?* | `internal/application.fallbackDecision` |

`SourceUnusable` is deliberately **wider** than `Degradable`. A schema-version
mismatch is terminal — retrying cannot help, and no amount of waiting makes an
incompatible contract compatible — so it is not degradable; but it is still a
reason to try the other source, in exactly the way a timeout is. Conversely
`NOT_FOUND` is neither: asking a second source about a resource the first
authoritatively does not have would turn a correct answer into a wrong one.

They are independent. `NOT_FOUND` is terminal, not retryable, not degradable and
not source-unusable (serving stale data for a deleted resource is wrong, and so
is asking someone else). `UPSTREAM_INVALID_RESPONSE` and
`SCHEMA_VERSION_MISMATCH` are terminal and not retryable and **not** degradable,
but they **are** source-unusable, so the call-time fallback runs for them.
`RATE_LIMITED` from an upstream is retryable but only under an externally
supplied delay.

### 3.1 Classification table

`Class` in `pkg/errs` is a single value per code, so "terminal" and "degradable"
are two names for positions on one axis: `ClassClient`, `ClassTransient`,
`ClassDegradable`, `ClassTerminal`.

| Code | Class | Retryable | Degradable | SourceUnusable | Breaker counts it | HTTP | Client `retryable` |
|---|---|---|---|---|---|---|---|
| `INVALID_REQUEST` | client | no | no | no | **abstains**² | 400 | false |
| `UNAUTHENTICATED` | client | no | no | no | **abstains**² | 401 | false |
| `FORBIDDEN` | client | no | no | no | **abstains**² | 403 | false |
| `TENANT_MISMATCH` | client | no | no | no | **abstains**² | 403 | false |
| `NOT_FOUND` | terminal | no | no | no | **abstains**² | 404 | false |
| `RATE_LIMITED` | transient | **yes** | no | no | yes | 429 | true |
| `REQUEST_TOO_LARGE` | client | no | no | no | **abstains**² | 413 | false |
| `UPSTREAM_TIMEOUT` | degradable | **yes** | **yes** | **yes** | yes | 504 | true |
| `UPSTREAM_UNAVAILABLE` | degradable | **yes** | **yes** | **yes** | yes | 503 | true |
| `UPSTREAM_INVALID_RESPONSE` | terminal | no | no | **yes** | yes | 502 | false |
| `SCHEMA_VERSION_MISMATCH` | terminal | no | no | **yes** | **abstains**¹ | 502 | false |
| `NO_SOURCE_AVAILABLE` | terminal | no³ | no⁴ | no | n/a | 503 | **true**⁵ |
| `INTERNAL` | terminal | no | no | no | yes | 500 | false |

¹ A schema mismatch does **not** trip the circuit breaker.
`resilience.isClientFault` lists it alongside the caller-fault codes, so the
breaker records it as a success. The source is up, fast and answering correctly;
it is the BFF that does not understand the contract. Counting it as a health
failure would trip a breaker on a healthy source, quietly reroute every request
to the slower one, and replace a loud version-incompatibility alert with a vague
availability one. It surfaces through `schema_version_mismatch_total`, a `502`
where no fallback exists, and the call-time fallback where one does
(REQ-EDGE-017).

² **Abstain, not "recorded as a success".** `Breaker.Do` returns without calling
`Record` at all for a client fault. Recording a success would be just as wrong as
recording a failure: a source answering nothing but 404s while it is genuinely
down would accumulate successes, satisfy `half_open_successes` and be re-admitted
to full traffic. Abstaining leaves the rolling window untouched, so the breaker's
verdict rests only on outcomes that are actually evidence about the source.

³ `NO_SOURCE_AVAILABLE` is not retried *by the BFF* — it is the terminus of the
degradation ladder, so an internal retry would repeat a completed search.

⁴ It is not `ClassDegradable`, but `Service.serveStale` admits it explicitly
alongside the `errs.SourceUnusable` causes: the whole point of that rung is to
answer when routing has already concluded that nothing can.

⁵ The client-facing `error.retryable` is `errs.IsRetryable`, derived from the code
alone: `UPSTREAM_TIMEOUT`, `UPSTREAM_UNAVAILABLE`, `RATE_LIMITED` **and**
`NO_SOURCE_AVAILABLE` report `true`. The last is terminal for *this* attempt but
the condition that produced it is an outage, and outages end — and the response
advertises `Retry-After`, so the hint has to agree with the header.

**Verified by** `TestErrs_ClassificationTable` (`pkg/errs/classify_test.go`),
`TestRetry_EligibilityTable`
(`internal/resilience/retry_eligibility_test.go`).

### 3.2 Why `INTERNAL` is not retryable

An internal error means the BFF's own state or logic failed. Retrying executes
the same code over the same inputs. It is not marked terminal either, because
the condition may be a transient resource exhaustion whose recurrence the client
should be free to test on its own schedule.

### 3.3 Why `UPSTREAM_INVALID_RESPONSE` is source-unusable but not retryable

A malformed payload is deterministic for that source state: retrying re-fetches
the same malformed record, so it is `ClassTerminal` and never retried. But
*another source* may well be able to answer, so `errs.SourceUnusable` returns
true for it and the call-time fallback runs. That is what converts a source-side
data bug into a degraded-but-correct response rather than an outage
(REQ-EDGE-020).

Note the asymmetry with the stale-cache rung: because the code is terminal
rather than degradable, `errs.IsDegradable` is false and `serveStale` will
**not** fire for it. A malformed payload from the only source that can answer is
a `502`, not a stale serve.

---

## 4. Adapter error mapping

### 4.1 gRPC (ODS) → code

| gRPC code | `errs.Code` | Retryable | Breaker counts |
|---|---|---|---|
| `UNAVAILABLE` | `UPSTREAM_UNAVAILABLE` | yes | yes |
| `DEADLINE_EXCEEDED` | `UPSTREAM_TIMEOUT` | yes (budget permitting) | yes |
| `RESOURCE_EXHAUSTED` | `UPSTREAM_UNAVAILABLE` | yes | yes |
| `ABORTED` | `UPSTREAM_UNAVAILABLE` | yes | yes |
| `INTERNAL` | `UPSTREAM_UNAVAILABLE` | **no** | yes |
| `UNKNOWN` | `UPSTREAM_UNAVAILABLE` | no | yes |
| `DATA_LOSS` | `UPSTREAM_INVALID_RESPONSE` | no | yes |
| `NOT_FOUND` | `NOT_FOUND` | no | **no** |
| `INVALID_ARGUMENT` | `INVALID_REQUEST` | no | no |
| `FAILED_PRECONDITION` | `UPSTREAM_INVALID_RESPONSE` | no | no |
| `OUT_OF_RANGE` | `INVALID_REQUEST` | no | no |
| `PERMISSION_DENIED` | `FORBIDDEN` | no | no |
| `UNAUTHENTICATED` | `UNAUTHENTICATED` | no | no |
| `UNIMPLEMENTED` | `SCHEMA_VERSION_MISMATCH` | no | **abstains** (see §3.1 notes ¹ and ²) |
| `CANCELED` (client-initiated) | *(propagated, not an upstream error)* | no | **no** |
| proto decode failure | `UPSTREAM_INVALID_RESPONSE` | no | yes |
| message exceeds `MaxCallRecvMsgSize` | `UPSTREAM_INVALID_RESPONSE` | no | yes |

### 4.2 HTTP (EDS) → code

| HTTP / condition | `errs.Code` | Retryable | Breaker counts |
|---|---|---|---|
| `400` | `INVALID_REQUEST` | no | no |
| `401` | `UNAUTHENTICATED` | no | no |
| `403` | `FORBIDDEN` | no | no |
| `404` | `NOT_FOUND` | no | **no** |
| `408` | `UPSTREAM_TIMEOUT` | yes | yes |
| `409`, `422` | `UPSTREAM_INVALID_RESPONSE` | no | no |
| `413` | `REQUEST_TOO_LARGE` | no | no |
| `429` | `RATE_LIMITED` | conditionally (see §6.2) | no |
| `500`, `502` | `UPSTREAM_UNAVAILABLE` | **no** | yes |
| `503` | `UPSTREAM_UNAVAILABLE` | yes | yes |
| `504` | `UPSTREAM_TIMEOUT` | yes | yes |
| other `4xx` | `UPSTREAM_INVALID_RESPONSE` | no | no |
| other `5xx` | `UPSTREAM_UNAVAILABLE` | no | yes |
| connection refused / DNS / TLS handshake | `UPSTREAM_UNAVAILABLE` | yes | yes |
| mid-stream reset, truncated body | `UPSTREAM_UNAVAILABLE` | yes | yes |
| malformed JSON, wrong content type | `UPSTREAM_INVALID_RESPONSE` | no | yes |
| empty body with `200` | `UPSTREAM_INVALID_RESPONSE` | no | yes |
| body exceeds 4 MiB | `UPSTREAM_INVALID_RESPONSE` | no | yes |
| context deadline (BFF-side) | `UPSTREAM_TIMEOUT` | budget permitting | yes |
| context cancelled (client) | *(propagated)* | no | **no** |

**Verified by** `TestAdapters_ErrorTranslation`
(`internal/datasource/errors_test.go`).

### 4.3 `500`/`502` are not retried; `503`/`504` are

A `500` or `502` means the upstream (or its proxy) executed and failed
deterministically. Retrying a deterministic failure doubles the load on an
already-failing service and cannot succeed. `503` and `504` explicitly signal
capacity or timing — conditions that a jittered retry can genuinely resolve. The
same logic explains why gRPC `INTERNAL` is not retried while `UNAVAILABLE` is.

### 4.4 Internal (non-adapter) errors

| Origin | `errs.Code` | Notes |
|---|---|---|
| breaker open | `UPSTREAM_UNAVAILABLE` + `SourceState: CIRCUIT_OPEN` | never dispatched; surfaces before the call (REQ-RES-006) |
| bulkhead acquire timeout | `UPSTREAM_UNAVAILABLE` + `SourceState: SATURATED` | degradable |
| source dropped below `min_viable_timeout` | `UPSTREAM_TIMEOUT` + warning | not dispatched (REQ-RT-006) |
| mapper mandatory-field failure | `UPSTREAM_INVALID_RESPONSE` | REQ-MAP-008 |
| mapper tenant assertion failure | `TENANT_MISMATCH` | + audit event (REQ-SEC-006) |
| major schema mismatch | `SCHEMA_VERSION_MISMATCH` | breaker untouched; the call-time fallback `fallback.primary_failed` handles it, else `502` (REQ-EDGE-017) |
| degradation ladder exhausted | `NO_SOURCE_AVAILABLE` | REQ-RES-009 |
| JWT invalid / expired | `UNAUTHENTICATED` | REQ-SEC-002 |
| `X-Tenant-ID` ≠ claim | `TENANT_MISMATCH` | REQ-SEC-004 |
| RBAC denial | `FORBIDDEN` | REQ-SEC-005 |
| path param / `limit` / `cursor` validation | `INVALID_REQUEST` | REQ-SEC-009 |
| body/header size | `REQUEST_TOO_LARGE` | REQ-SEC-010 |
| inbound rate limit | `RATE_LIMITED` + `Retry-After` | REQ-RES-008 |
| recovered panic | `INTERNAL` | REQ-RES-012 |
| encoding failure | `INTERNAL` | |

---

## 5. Degradation decision table

The single most important table in this document. It determines whether a
failure produces `200`-degraded, `206`-partial, or an error status
(REQ-API-008, REQ-AGG-004, REQ-RES-009).

### 5.1 Vocabulary

| Term | Meaning |
|---|---|
| **required source** | `Decision.RequiredSources[s] == true` |
| **optional source** | `Decision.RequiredSources[s] == false` — absence yields `partial` |
| **degraded** | data is stale, cached-stale, or from a fallback source |
| **partial** | at least one field group could not be obtained |
| **usable candidate** | fresh cache, or stale cache within `max_stale` with `allow_stale` and `Consistency != STRONG` |

### 5.2 Single-source decisions (`Target = OPERATIONAL` or `EXECUTION`)

| # | Source outcome | Fallback usable | Cache candidate | `allow_stale` & in-bound | Status | degraded | partial | Warning |
|---|---|---|---|---|---|---|---|---|
| 1 | success, fresh | — | — | — | 200 | false | false | — |
| 2 | success, stale within `max_stale` | — | — | yes | 200 | **true** | false | `STALE_DATA` |
| 3 | success, stale beyond `max_stale` | no | none | — | 503 `NO_SOURCE_AVAILABLE` | true | — | — |
| 4 | timeout (required) | yes | — | — | 200, or **206** when the fallback cannot supply every requested field | true | maybe | `SOURCE_TIMEOUT` |
| 5 | timeout (required) | no | fresh | — | 200 | false | false | — |
| 6 | timeout (required) | no | stale | yes | 200 | **true** | false | `STALE_DATA` |
| 7 | timeout (required) | no | none | — | **504** `UPSTREAM_TIMEOUT` | — | — | — |
| 8 | unavailable / breaker open (required) | yes | — | — | 200, or **206** when the fallback cannot supply every requested field | true | maybe | `SOURCE_UNAVAILABLE` (naming the source that **failed**) |
| 9 | unavailable (required) | no | stale usable | yes | 200 | **true** | false | `STALE_DATA` |
| 10 | unavailable (required) | no | none | — | **503** `UPSTREAM_UNAVAILABLE` | — | — | — |
| 11 | invalid payload (required) | no | stale usable | yes | 200 | **true** | false | `STALE_DATA` |
| 12 | invalid payload (required) | no | none | — | **502** `UPSTREAM_INVALID_RESPONSE` | — | — | — |
| 13 | schema major mismatch (required) | yes | — | — | 200, or **206** when the fallback cannot supply every requested field | true | maybe | `SCHEMA_VERSION_MISMATCH` |
| 14 | schema major mismatch (required) | no | none | — | **502** `SCHEMA_VERSION_MISMATCH` | — | — | — |
| 15 | not found (all consulted) | — | — | — | **404** `NOT_FOUND` (negatively cached) | — | — | — |
| 16 | success, empty collection | — | — | — | 200 | false | false | — (no warning; an empty collection is an answer) |
| 17 | `Consistency = STRONG`, source failed | — | any | — | 503/504 per cause | — | — | stale never served |

### 5.3 Fan-out decisions (`Target = BOTH`)

`R` = required source outcome, `O` = optional source outcome.

| # | R (operational) | O (execution) | Status | degraded | partial | Notes |
|---|---|---|---|---|---|---|
| 1 | success fresh | success | 200 | false | false | the happy path |
| 2 | success fresh | timeout | **200** | false | **true** | `SOURCE_TIMEOUT`; R was never cancelled (REQ-AGG-002) |
| 3 | success fresh | unavailable | **200** | false | **true** | `SOURCE_UNAVAILABLE` |
| 4 | success fresh | invalid payload | **200** | false | **true** | `PARTIAL_DATA` |
| 5 | success stale (in bound) | success | 200 | **true** | false | `STALE_DATA` |
| 6 | success stale (in bound) | timeout | 200 | **true** | **true** | both flags — orthogonal (REQ-API-007) |
| 7 | timeout | success | **206** | true | true | a required group is missing but O supplied some |
| 8 | unavailable | success | **206** | true | true | provenance shows only EXECUTION groups |
| 9 | invalid payload | success | **206** | true | true | |
| 10 | timeout | timeout | 503/504 | — | — | unless a usable cache candidate exists ⇒ 200 degraded |
| 11 | unavailable | unavailable | **503** `NO_SOURCE_AVAILABLE` | — | — | rule `health.both_unavailable` usually pre-empts dispatch |
| 12 | success | not found | 200 | false | false | — (no warning); `latestExecution` absent is legitimate |
| 13 | not found | not found | **404** | — | — | the resource genuinely does not exist |
| 14 | not found | success | 200 | false | **true** | execution records exist for an unknown operational record — a data-quality signal, warn `PARTIAL_DATA` |

### 5.4 The 200 / 206 / 503 rule, stated once

```
if no field group was obtained:
    → error status from the dominant cause (§5.5)
else if a REQUIRED field group is missing:
    → 206, partial = true
else if only OPTIONAL groups are missing:
    → 200, partial = true
else:
    → 200, partial = false

degraded = (data was stale) OR (data came from a fallback source)
           OR (data came from a stale cache entry)

partial  = (a source the request wanted did not answer)
           OR (the source that DID answer holds none of the suppliers
               for some requested field)
```

The second `partial` clause is what makes a fallback answer honest. A field is
unsatisfiable only when *none* of its catalogued suppliers is in the chosen
target, so `/resources/{id}` answered by the EDS is partial (no `configuration`,
`owner`, `metrics`, `topology`) while `/status` answered by the EDS is not
(`status` lists the EDS as a supplier).

`degraded` and `partial` are computed independently and may both be true
(REQ-API-007). Neither affects the other's value.

### 5.5 Dominant-cause selection

When a **fallback** path fails, there is no selection to make: the **primary's**
error is reported and the fallback's is logged at warn and discarded. The
fallback's error describes a source the caller never asked about, and a
`NOT_FOUND` from the execution source means only that the resource has no
execution history — returning it would turn a transient operational outage into a
`404` and then negatively cache that `404` for a resource that exists. Reporting
the primary's error is what keeps a fallback-path `NOT_FOUND` out of the negative
cache.

When several sources failed differently *within one fan-out*, the emitted error is
chosen by this precedence, so the client sees the most actionable cause:

```
TENANT_MISMATCH  >  SCHEMA_VERSION_MISMATCH  >  UPSTREAM_INVALID_RESPONSE
                 >  UPSTREAM_TIMEOUT         >  UPSTREAM_UNAVAILABLE
                 >  NO_SOURCE_AVAILABLE
```

Rationale: security and contract failures are actionable by an operator and must
not be masked by a concurrent transient failure; timeouts are more specific than
generic unavailability; `NO_SOURCE_AVAILABLE` is the fallback when nothing more
specific applies. `error.sources[]` always lists **every** consulted source with
its individual state, so the discarded causes remain visible.

**Verified by** `TestStatusMapping_Table` (`internal/api/response/status_test.go`),
`TestAggregate_PartialMatrix` (`internal/aggregation/partial_test.go`),
`TestDegradation_Ladder` (`internal/application/degradation_test.go`).

---

## 6. Retry eligibility

### 6.1 The rule: never retry blindly

Retry is permitted only when **all** of the following hold (REQ-RES-003):

1. `err.Retryable == true` per §3.1 — decided by the typed classification, never
   by "an error occurred" or by string inspection.
2. The operation is a read. All ports are reads (REQ-DS-008), so this is
   structurally satisfied, but the check exists so that adding a write later
   fails closed.
3. Attempts so far `< retry.max_attempts` (total, not additional).
4. The remaining request deadline can accommodate the backoff **plus** a full
   attempt at its per-attempt timeout. An attempt that cannot complete is not
   made — it would burn a connection and a bulkhead permit to produce a
   guaranteed cancellation (REQ-RES-002, REQ-RT-006).
5. The breaker for that source is not `OPEN`, and the request holds a bulkhead
   permit.
6. The error is not a client cancellation (REQ-RES-011).

Failing any condition ends the attempt sequence and hands the error to the
degradation ladder.

### 6.2 `RATE_LIMITED` is the one conditional case

An upstream `429` is retried **only** when it supplies `Retry-After` and that
delay fits inside the remaining deadline. Without a hint, or with one that does
not fit, the error is passed to the ladder immediately. Ignoring `Retry-After`
and applying the local backoff schedule would amplify an overload the upstream
is explicitly asking us to relieve.

### 6.3 Backoff schedule

```
delay(n) = rand(0, min(max_backoff, base_backoff × 2^(n−1)))     // full jitter
```

Full jitter, not equal jitter and not fixed backoff. The alternatives
resynchronize every client that failed at the same instant, producing a
thundering herd exactly when the upstream is most fragile. The distribution
property is asserted directly by `TestRetry_FullJitterDistribution` rather than
being assumed from the code shape.

| Source | `max_attempts` | `base_backoff` | `max_backoff` |
|---|---|---|---|
| operational | 3 | 20ms | 200ms |
| execution | 2 | 50ms | 400ms |

The EDS gets fewer attempts because at a 240 ms P95 a third attempt rarely fits a
per-source budget, and its bulkhead is half as wide.

### 6.4 Operations that are never retried

| Operation | Reason |
|---|---|
| `GetResourceFreshness` (the probe) | a retry costs more than the full read it exists to avoid (REQ-TTL-005) |
| any call whose error is `Terminal` | it will fail identically |
| any call after the breaker opened mid-sequence | the breaker's decision supersedes the retry budget |
| any call for a cancelled request | the client is gone (REQ-RES-011) |
| the degradation ladder itself | it is a search over alternatives, not a repeat of one call |
| fallback after a fallback | single-hop only (REQ-RT-008) |

### 6.5 Interaction with the circuit breaker

Retry sits **inside** the breaker and **inside** the bulkhead:

```
bulkhead.Acquire → breaker.Execute( retry( timeout( call ) ) )
```

Consequences, all deliberate:

- A retried-and-eventually-failed call registers **one** logical failure with the
  breaker, not `max_attempts` failures. Counting each attempt would make the
  breaker trip `max_attempts` times faster than its configured ratio implies.
- Retries do not acquire a second bulkhead permit, so a retry storm cannot
  consume the concurrency budget.
- Once the breaker opens, in-flight retry sequences stop at their next attempt
  boundary rather than running to exhaustion.

**Verified by** `TestRetry_MaxAttempts`, `TestRetry_RespectsDeadline`,
`TestBreaker_StateMachine`, `TestBulkhead_CapsConcurrency`.

---

## 7. Error response construction

### 7.1 Shape

```jsonc
{
  "error": {
    "code": "UPSTREAM_UNAVAILABLE",
    "type": "https://errors.bff.internal/upstream-unavailable",
    "title": "Upstream data source unavailable",
    "status": 503,
    "detail": "Execution source is required for this endpoint and is not serving.",
    "correlationId": "aa112233-4455-4667-8889-9aabbccddeef",
    "retryable": true,
    "sources": [ { "source": "EXECUTION", "state": "CIRCUIT_OPEN" } ]
  }
}
```

| Field | Source | Rule |
|---|---|---|
| `code` | `errs.Error.Code` | closed set (REQ-API-009) |
| `type` | derived | `https://errors.bff.internal/<kebab-code>` |
| `title` | static per code | never includes request-specific data |
| `status` | §3.1 mapping | |
| `detail` | `errs.Error.Public` | never `Message`; never a stack trace, upstream hostname, raw payload or token content (REQ-SEC-013) |
| `correlationId` | request context | the key to the full internal detail in the logs |
| `retryable` | §3.1 client column | client-facing hint, distinct from the internal `Retryable` |
| `sources[]` | per-source outcome record | **every** consulted source with its state, including ones that succeeded |

`sources[]` deliberately lists successes too. During an incident, "the ODS was
`SERVING` and the EDS was `CIRCUIT_OPEN`" is a materially different diagnosis
from "both were `CIRCUIT_OPEN`", and the response should say which.

### 7.2 Headers on error responses

| Header | When |
|---|---|
| `X-Correlation-ID` | always |
| `Retry-After` | `429`, and `503`/`504` when a sensible hint exists |
| `WWW-Authenticate` | `401` |
| `Allow` | `405` |
| `Cache-Control: no-store` | always |

### 7.3 What is logged instead

The full internal detail — `errs.Error.Message`, the `Cause` chain, the truncated
and redacted upstream payload for `UPSTREAM_INVALID_RESPONSE`, the routing
`Decision`, per-source latencies — is written to the request log line keyed by
`correlation_id` (REQ-OBS-005), and the failing span records the exception with
`error.code` (REQ-OBS-004). The client gets a code and a correlation id; an
operator gets everything.

**Verified by** `TestErrors_NoInternalLeakage`
(`internal/api/response/error_test.go`),
`TestLogging_RedactsSecrets` (`internal/security/redaction_test.go`).

---

## 8. Negative caching and errors

| Outcome | Cached? | TTL | Req |
|---|---|---|---|
| `NOT_FOUND` confirmed by every consulted source | **yes** | `cache.negative_ttl` (3s) | REQ-CACHE-005 |
| `NOT_FOUND` from one source while another was unavailable | **no** | — | absence is unconfirmed |
| empty collection (`200` with `[]`) | as a normal positive entry | `cache_ttl` | never as not-found (REQ-EDGE-019) |
| `UPSTREAM_*`, `RATE_LIMITED`, `INTERNAL` | **no** | — | caching a transient failure extends the outage |
| `UNAUTHENTICATED`, `FORBIDDEN`, `TENANT_MISMATCH` | **no** | — | caching an authz decision across token states is a security hazard |
| `SCHEMA_VERSION_MISMATCH` | **no**, and the source is **not** marked unavailable — the breaker never sees it | — | REQ-EDGE-017 |

**Verified by** `TestCache_NegativeCaching`,
`TestCache_ErrorsNotNegativelyCached`.

---

## 9. Error metrics and alert surface

| Metric | Attributes | Answers |
|---|---|---|
| `bff_request_total` | `outcome`, `http_status`, `request_type`, `tenant_id` | client-visible error rate per endpoint |
| `datasource_error_total` | `source`, `outcome` ∈ `timeout\|unavailable\|invalid\|not_found\|schema_mismatch` | which source is failing and how |
| `circuit_breaker_transition_total` | `source`, `state` | when protection engaged |
| `partial_response_total` | `request_type`, `source` | how often optional data is missing |
| `stale_response_total` | `request_type`, `source` | how often the degradation ladder reached the stale step |

Cardinality rules apply: `resourceId`, `executionId` and `correlationId` are
never metric attributes (REQ-OBS-002).

Suggested alerts (detail in `spec/observability.md`):

| Condition | Meaning |
|---|---|
| `NO_SOURCE_AVAILABLE` rate > 0.1% for 5m | the degradation ladder is exhausting — a real outage |
| `UPSTREAM_INVALID_RESPONSE` rate > 0 sustained | a source shipped a breaking change; not self-healing |
| `SCHEMA_VERSION_MISMATCH` > 0 | a major contract break; page immediately |
| `TENANT_MISMATCH` > 0 | a source returned foreign-tenant data; security incident |
| breaker `OPEN` for > 2 cooldowns | the upstream is not recovering |
| `partial_response_total` / `bff_request_total` > 5% | an optional source is effectively down |

---

## 10. Test obligations

| Obligation | Test | Req |
|---|---|---|
| Code set closed and matches the contract + OpenAPI | `TestErrorCodes_ClosedSet` | REQ-API-009 |
| `type` URI derivation | `TestErrorType_URIDerivation` | REQ-API-009 |
| Classification table exact | `TestErrs_ClassificationTable` | REQ-RES-004 |
| Retry eligibility table exact | `TestRetry_EligibilityTable` | REQ-RES-003 |
| Full-jitter distribution | `TestRetry_FullJitterDistribution` | REQ-RES-002 |
| Retry never exceeds the deadline | `TestRetry_RespectsDeadline` | REQ-RES-002 |
| Adapter translation for every row of §4.1/§4.2 | `TestAdapters_ErrorTranslation` | REQ-DS-007 |
| Status mapping for every row of §5.2/§5.3 | `TestStatusMapping_Table` | REQ-API-008 |
| Degradation ladder order | `TestDegradation_Ladder` | REQ-RES-009 |
| `degraded`/`partial` orthogonality | `TestMeta_DegradedPartialOrthogonal` | REQ-API-007 |
| No internal leakage in `detail` | `TestErrors_NoInternalLeakage` | REQ-SEC-013 |
| Client cancellation not counted by the breaker | `TestCancellation_NotCountedByBreaker` | REQ-RES-011 |
| Negative caching scope | `TestCache_NegativeCaching`, `TestCache_ErrorsNotNegativelyCached` | REQ-CACHE-005 |
| Panic containment | `TestRecovery_HandlerPanic`, `TestRecovery_GoroutinePanic` | REQ-RES-012 |
