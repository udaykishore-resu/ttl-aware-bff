# TTL-Aware BFF — Test Strategy

The complete verification plan: unit test matrix by package and requirement,
integration tests, contract tests for both sources, failure and chaos tests, load
tests with k6 scenarios and thresholds, coverage targets, and how the reference
sources' chaos knobs are driven.

Every requirement in `spec/requirements.md` names a test; this document is the
inverse index and the operational plan for running them.

---

## 1. Strategy

### 1.1 Principles

1. **Requirements are the unit of verification.** A test exists because a
   requirement demands it. A test with no `REQ-` id is either a missing
   requirement or an unnecessary test.
2. **Pure policy is table-tested; impure edges are faked; the whole is
   integration-tested.** The router, freshness manager, precedence policy,
   classifier and mappers are pure functions (REQ-RT-004, REQ-MAP-007), so they
   are exhaustively table-tested with no fakes at all. Adapters are tested against
   the reference stubs. The lifecycle is tested end-to-end.
3. **Time is injected, never read.** A `Clock` interface is a parameter
   everywhere a duration or age is computed. No test sleeps to advance logical
   time; sleeps appear only where real concurrency is under test.
4. **The twenty edge cases are integration tests, not unit tests.** They are
   properties of the assembled system and are driven through the real HTTP
   surface against the reference sources with chaos applied.
5. **Determinism is itself a requirement** (REQ-API-012), so golden-file tests
   are viable and are used for the envelope and the mappers.
6. **`goleak` on every package with goroutines.** REQ-AGG-005 is not
   self-enforcing.

### 1.2 Test pyramid and where the weight sits

| Layer | Count (approx) | Runtime | Gate |
|---|---|---|---|
| Unit (table/property/golden) | ~600 | < 30 s | every commit |
| Contract (vs. reference stubs) | ~90 | < 60 s | every commit |
| Integration (in-process, real HTTP + stubs) | ~120 | < 4 min | every commit |
| Failure / chaos | ~45 | < 6 min | every commit |
| Load (k6) | 6 scenarios | 5–30 min | nightly + pre-release |
| Soak | 1 scenario | 4 h | pre-release |

The weight is deliberately in unit and integration rather than end-to-end: the
policy layer is where the interesting decisions live, and it is cheap to test
exhaustively because it is pure.

---

## 2. Unit test matrix

Format: package · behaviour · requirement · test.

### 2.1 `internal/domain`

| Behaviour | Req | Test |
|---|---|---|
| Imports nothing from this module or any transport package | REQ-MAP-001 | `TestDomain_NoOutboundImports` |
| Enum sets match the contract exactly | REQ-MAP-003/004 | `TestDomain_EnumSets` |
| Optional structures are pointers so absent ≠ zero | REQ-PREC-005 | `TestDomain_OptionalPointerDiscipline` |

### 2.2 `internal/classifier`

| Behaviour | Req | Test |
|---|---|---|
| Route template → `RequestType` | REQ-CLS-001 | `TestClassifier_RouteToRequestType` |
| Totality over the registered route set | REQ-CLS-002 | `TestClassifier_TotalOverRoutes` |
| `RequiredFields` per request type | REQ-CLS-003 | `TestClassifier_RequiredFieldsPerType` |
| `Consistency = STRONG` from `Cache-Control: no-cache` | REQ-CLS-004 | `TestClassifier_ConsistencyFromNoCache` |
| `Consistency = STRONG` from `allow_stale: false` | REQ-CLS-004 | `TestClassifier_ConsistencyFromConfig` |
| Span/metric attributes emitted | REQ-CLS-005 | `TestClassifier_SpanAttributes` |

### 2.3 `internal/freshness`

| Behaviour | Req | Test |
|---|---|---|
| Verdict table (`FRESH`/`STALE`/`UNKNOWN`, `ttl = 0`) | REQ-TTL-002 | `TestFreshness_VerdictTable` |
| Age computed in the source clock domain | REQ-TTL-004 | `TestFreshness_SameClockDomain` |
| Two-clock fallback clamps skew and biases older | REQ-TTL-004 | `TestFreshness_SkewClamped` |
| Negative age clamped to 0 + warning | REQ-TTL-004 | `TestFreshness_NegativeAgeClamped` |
| Gross skew forces `UNKNOWN` | REQ-TTL-004 | `TestFreshness_GrossSkewForcesUnknown` |
| Probe has its own timeout | REQ-TTL-005 | `TestProbe_OwnTimeout` |
| Probe failure ⇒ `UNKNOWN`, not an error | REQ-TTL-005 | `TestProbe_FailureYieldsUnknown` |
| Memo stores the observation, not the verdict | REQ-TTL-006 | `TestProbe_MemoStoresObservationNotVerdict` |
| Stale bounded by `max_stale` | REQ-TTL-008 | `TestStale_BoundedByMaxStale` |
| Cache TTL ≠ freshness TTL | REQ-TTL-001 | `TestTTL_CacheTTLDistinctFromFreshnessTTL` |
| Age histogram recorded | REQ-TTL-009 | `TestFreshness_AgeHistogramRecorded` |

### 2.4 `internal/router`

| Behaviour | Req | Test |
|---|---|---|
| Chain order matches the contract | REQ-RT-001 | `TestRouter_ChainOrder` |
| First match wins | REQ-RT-001 | `TestRouter_FirstMatchWins` |
| Every decision carries a rule id (property test over randomized inputs) | REQ-RT-002 | `TestRouter_AlwaysEmitsRuleID` |
| Probe precedes any full read | REQ-RT-003 | `TestRouter_ProbeBeforeFullRead` |
| A full read is never used purely for freshness | REQ-RT-003 | `TestRouter_NoFullReadForFreshnessOnly` |
| Decision is pure (no I/O, no mutation) | REQ-RT-004 | `TestRouter_DecisionIsPure` |
| `Reason` includes deciding operands | REQ-RT-005 | `TestRouter_ReasonIncludesInputs` |
| Sources below `min_viable_timeout` are dropped, not called | REQ-RT-006 | `TestRouter_DropsSourceBelowMinViableTimeout` |
| `RequiredSources` from config | REQ-RT-007 | `TestRouter_RequiredSourcesFromConfig` |
| Fallback is single-hop | REQ-RT-008 | `TestRouter_FallbackSingleHop` |
| Decision metric emitted | REQ-RT-009 | `TestRouter_EmitsDecisionMetric` |
| One health snapshot for the whole chain | REQ-RT-010 | `TestRouter_HealthSnapshotStability` |
| `on_unknown_freshness` honoured | REQ-TTL-007 | `TestRouter_OnUnknownFreshnessConfigurable` |
| Missing tenant fails closed | REQ-MT-001 | `TestTenant_MissingFailsClosed` |
| Truth tables in `spec/routing-policy.md` §5 reproduced exactly | REQ-RT-001 | `TestRouter_TruthTable` |

### 2.5 `internal/policy`

| Behaviour | Req | Test |
|---|---|---|
| Per-field-group resolution | REQ-PREC-001 | `TestPrecedence_PerFieldGroup` |
| Default table matches the contract | REQ-PREC-002 | `TestPrecedence_DefaultTableMatchesContract` |
| Execution overrides while running | REQ-PREC-003 | `TestPrecedence_ExecutionOverridesWhenRunning` |
| No warning when the sources agree | REQ-PREC-003 | `TestPrecedence_NoWarningWhenAgreeing` |
| Conflicts counted and warned | REQ-PREC-004 | `TestPrecedence_ConflictCounted` |
| Empty never displaces populated | REQ-PREC-005 | `TestPrecedence_EmptyDoesNotDisplace` |
| Timestamp is not a tiebreaker | REQ-PREC-006 | `TestPrecedence_TimestampNotTiebreaker` |
| Provenance names the winner | REQ-API-006 | `TestProvenance_ReflectsWinningSource` |

### 2.6 `internal/mapper`

| Behaviour | Req | Test |
|---|---|---|
| ODS enum totality | REQ-MAP-002 | `TestMapper_EnumTotality_Operational` |
| EDS enum totality | REQ-MAP-002 | `TestMapper_EnumTotality_Execution` |
| Unknown enum ⇒ `UNKNOWN` + drift warning | REQ-MAP-002 | `TestMapper_UnknownEnumWarns` |
| `ResourceState` table | REQ-MAP-003 | `TestMapper_ResourceStateTable` |
| Execution status table | REQ-MAP-004 | `TestMapper_ExecutionStatusTable` |
| Rename table | REQ-MAP-005 | `TestMapper_FieldRenameTable` |
| Completeness vs the declared drop list | REQ-MAP-006 | `TestMapper_CompletenessAgainstDropList` |
| Purity (no I/O, no global clock, no logging) | REQ-MAP-007 | `TestMapper_Purity` |
| Malformed payloads rejected | REQ-MAP-008 | `TestMapper_RejectsMalformed` |
| Optional fields defaulted as declared | REQ-MAP-008 | `TestMapper_DefaultsOptional` |
| Units preserved, never converted | REQ-MAP-009 | `TestMapper_UnitsPreserved` |
| Zero timestamp is absent, not epoch | REQ-MAP-010 | `TestMapper_ZeroTimestampIsAbsent` |
| Foreign-tenant record rejected | REQ-SEC-006 | `TestTenant_ResponseTenantAsserted` |
| `lastOperation` derivation | — | `TestMapper_DeriveLastOperation` |
| Golden fixtures for both sources | REQ-API-012 | `TestMapper_GoldenFixtures` |

### 2.7 `internal/aggregation`

| Behaviour | Req | Test |
|---|---|---|
| Branches start concurrently | REQ-AGG-001 | `TestAggregate_ConcurrentStart` |
| Independent per-source deadlines | REQ-AGG-001 | `TestAggregate_IndependentDeadlines` |
| Optional failure does not cancel required | REQ-AGG-002 | `TestAggregate_OptionalFailureDoesNotCancelRequired` |
| Per-source outcome record | REQ-AGG-003 | `TestAggregate_PerSourceOutcome` |
| Partial matrix (§5.3 of the error model) | REQ-AGG-004 | `TestAggregate_PartialMatrix` |
| No goroutine leaks | REQ-AGG-005 | `TestAggregate_NoGoroutineLeak` |
| Latency is wall time of the fan-out | REQ-AGG-006 | `TestAggregate_LatencyIsWallTime` |
| No N+1 dispatch | REQ-AGG-007 | `TestAggregate_NoNPlusOne` |
| Fan-out is max, not sum | REQ-PERF-006 | `TestPerf_FanOutIsMaxNotSum` |

### 2.8 `internal/cache`

| Behaviour | Req | Test |
|---|---|---|
| L1 → L2 → source order | REQ-CACHE-001 | `TestCache_LayerOrder` |
| L2 failure degrades, never fails | REQ-CACHE-001 | `TestCache_L2FailureDegrades` |
| Key structure | REQ-CACHE-002 | `TestCacheKey_Structure` |
| Tenant separation, including hash collisions | REQ-MT-005 | `TestCacheKey_TenantSeparation`, `TestCache_NoCrossTenantRead` |
| Entry carries `observedAt` | REQ-CACHE-003 | `TestCache_EntryCarriesObservedAt` |
| Hit can still be stale | REQ-CACHE-009 | `TestCache_HitCanStillBeStale` |
| Singleflight collapses concurrent misses | REQ-CACHE-004 | `TestCache_SingleflightCollapses` |
| Concurrent same-resource requests ⇒ one upstream call | REQ-EDGE-012 | `TestCache_ConcurrentSameResource` |
| Negative caching of `NOT_FOUND` | REQ-CACHE-005 | `TestCache_NegativeCaching` |
| Errors not negatively cached | REQ-CACHE-005 | `TestCache_ErrorsNotNegativelyCached` |
| `STRONG` bypasses the read | REQ-CACHE-006 | `TestCache_StrongBypassesRead` |
| Schema version mismatch evicts | REQ-CACHE-007 | `TestCache_SchemaVersionMismatchEvicts` |
| L1 bounded LRU eviction | REQ-CACHE-008 | `TestCache_L1EvictionBounded` |
| Metrics emitted | REQ-CACHE-010 | `TestCache_MetricsEmitted` |

### 2.9 `internal/resilience`

| Behaviour | Req | Test |
|---|---|---|
| No unbounded contexts (static scan) | REQ-RES-001 | `TestResilience_NoUnboundedContext` |
| Full-jitter distribution | REQ-RES-002 | `TestRetry_FullJitterDistribution` |
| Retry fits the deadline | REQ-RES-002 | `TestRetry_RespectsDeadline` |
| Attempt cap | REQ-RES-002 | `TestRetry_MaxAttempts` |
| Eligibility table | REQ-RES-003 | `TestRetry_EligibilityTable` |
| Breaker state machine | REQ-RES-005 | `TestBreaker_StateMachine` |
| Half-open admits one probe | REQ-RES-005 | `TestBreaker_HalfOpenSingleProbe` |
| Transition metrics | REQ-RES-005 | `TestBreaker_MetricsOnTransition` |
| Open breaker feeds the health snapshot | REQ-RES-006 | `TestBreaker_OpenFeedsHealthSnapshot` |
| Bulkhead caps concurrency | REQ-RES-007 | `TestBulkhead_CapsConcurrency` |
| Bulkhead acquire timeout | REQ-RES-007 | `TestBulkhead_AcquireTimeout` |
| Per-tenant rate limit buckets | REQ-RES-008 | `TestRateLimit_PerTenantBucket` |
| `Retry-After` on rejection | REQ-RES-008 | `TestRateLimit_RetryAfterHeader` |
| Client cancel not counted by the breaker | REQ-RES-011 | `TestCancellation_NotCountedByBreaker` |

### 2.10 `internal/datasource`

| Behaviour | Req | Test |
|---|---|---|
| Freshness uses the probe RPC, never a full read | REQ-DS-002 | `TestOperationalAdapter_FreshnessUsesProbeRPC` |
| `RequestContext` populated on every call | REQ-DS-003 | `TestOperationalAdapter_PopulatesRequestContext` |
| EDS endpoint set exact | REQ-DS-004 | `TestExecutionAdapter_EndpointSet` |
| Fail-fast when unavailable (`WaitForReady(false)`) | REQ-DS-005 | `TestOperationalAdapter_FailFastWhenUnavailable` |
| HTTP transport pooling settings | REQ-DS-005 | `TestExecutionAdapter_TransportPooling` |
| Health snapshot read is non-blocking | REQ-DS-006 | `TestHealthPoller_SnapshotReadIsNonBlocking` |
| Error translation for every mapped code | REQ-DS-007 | `TestAdapters_ErrorTranslation` |
| All port methods are reads | REQ-DS-008 | `TestPorts_AllMethodsAreReads` |
| Response size bounded | REQ-DS-009 | `TestAdapters_ResponseSizeBounded` |
| Application depends on interfaces only | REQ-DS-001 | `TestPorts_ApplicationDependsOnInterfaces` |

### 2.11 `internal/api` and `internal/api/**`

| Behaviour | Req | Test |
|---|---|---|
| Endpoint set closed | REQ-API-001 | `TestRouter_EndpointSetIsClosed` |
| Method not allowed | REQ-API-001 | `TestRouter_MethodNotAllowed` |
| Admin not on the data plane | REQ-API-002 | `TestAdmin_NotServedOnDataPlane` |
| `/readyz` reflects sources; `/livez` does not | REQ-API-003 | `TestHealth_ReadyzReflectsSources`, `TestHealth_LivezIndependent` |
| A strongly-consistent request bypasses the cache **read** and still writes back | REQ-CACHE-006 | `TestCache_StrongBypassesRead` |
| A fallback answer that cannot cover the requested fields is `206` with `PARTIAL_DATA`; one that can stays `200` | REQ-EDGE-018 | `TestFallback_PartialWhenFieldsUncovered` |
| `on_unknown_freshness: none` yields `NONE`; only an unparseable value falls back to `operational` | REQ-TTL-006 | `TestRouter_UnknownFreshnessNone` |
| A client fault **abstains** from the breaker rather than counting as a success | REQ-RES-005 | `TestBreaker_ClientFaultAbstains` |
| Envelope shape stable | REQ-API-004 | `TestEnvelope_ShapeIsStable` |
| No source-native field names in output | REQ-API-004 | `TestEnvelope_NoSourceNativeFields` |
| `meta.freshness` always carries `ageSeconds` and `ttlSeconds`, and both round-trip through a cache entry; `UNKNOWN` is signalled by `state`, not by omitting the age | REQ-API-005 | `TestFreshness_JSONRoundTrip` (`internal/domain/domain_test.go`) |
| `degraded`/`partial` orthogonal | REQ-API-007 | `TestMeta_DegradedPartialOrthogonal` |
| Status mapping table | REQ-API-008 | `TestStatusMapping_Table` |
| Correlation round-trip and rejection of malformed | REQ-API-010 | `TestCorrelation_RoundTrip`, `TestCorrelation_RejectsMalformed` |
| Trace propagation inbound and outbound | REQ-API-011 | `TestTracePropagation_InboundOutbound` |
| Byte-identical golden responses | REQ-API-012 | `TestEnvelope_GoldenDeterminism` |
| Content type and timestamp format | REQ-API-013 | `TestEncoding_ContentTypeAndTimestamps` |
| `limit` below 1 or non-integer is `400`; above `max_history_items` is clamped down, not rejected | REQ-API-014 | `TestExecutions_LimitValidation` |
| Warning codes enumerated | REQ-API-015 | `TestWarnings_CodesEnumerated` |
| Middleware order | §14 of `spec/security.md` | `TestMiddleware_Order` |
| Panic containment | REQ-RES-012 | `TestRecovery_HandlerPanic`, `TestRecovery_GoroutinePanic` |
| Graceful drain | REQ-RES-010 | `TestShutdown_DrainsInFlight` |
| No internal leakage in errors | REQ-SEC-013 | `TestErrors_NoInternalLeakage` |

### 2.12 `internal/security`

Full matrix in `spec/security.md` §15: `TestAuth_RequiredOnDataPlane`,
`TestJWT_ValidationMatrix`, `TestJWT_RejectsAlgNone`, `TestJWT_LeewayApplied`,
`TestJWKS_RotationOnUnknownKid`, `TestJWKS_RefreshRateLimited`,
`TestTenant_ClaimIsAuthoritative`, `TestTenant_HeaderMismatchRejected`,
`TestTenant_EnforcementPoints`, `TestRBAC_EndpointPermissionMatrix`,
`TestRedaction_OutputFiltering`, `TestLogging_RedactsSecrets`,
`TestHeaders_SecurityDefaults`, `TestAudit_EventsEmitted`,
`TestValidation_PathParams`, `TestValidation_RejectsBeforeUpstream`,
`TestValidation_BodySizeLimit`.

### 2.13 `internal/config`

| Behaviour | Req | Test |
|---|---|---|
| No hard-coded durations in `internal/**` (AST scan) | REQ-CFG-001 | `TestConfig_NoHardcodedDurations` |
| Layer precedence file → env → tenant | REQ-CFG-002 | `TestConfig_LayerPrecedence` |
| `BFF_` prefix and `__` nesting | REQ-CFG-002 | `TestConfig_EnvNestingSeparator` |
| Validation matrix | REQ-CFG-003 | `TestConfig_ValidationMatrix` |
| `ttl: 0` forbids `cache_ttl > 0` | REQ-TTL-010 | `TestConfig_ZeroTTLRejectsPositiveCacheTTL` |
| Atomic reload | REQ-CFG-004 | `TestConfig_ReloadAtomicity` |
| Invalid reload keeps the previous snapshot | REQ-CFG-004 | `TestConfig_InvalidReloadKeepsPrevious` |
| Non-reloadable keys declared | REQ-CFG-005 | `TestConfig_NonReloadableKeys` |
| Starts with defaults, no config file | REQ-CFG-007 | `TestConfig_StartsWithDefaults` |
| Tenant overlay merge | REQ-MT-002 | `TestConfig_TenantOverlayMerge` |
| Unknown tenant uses defaults | REQ-MT-002 | `TestConfig_UnknownTenantUsesDefaults` |
| Effective TTL: tenant override wins | REQ-TTL-003 | `TestTTL_TenantOverrideWins` |
| Precedence tenant override validated | REQ-PREC-007 | `TestPrecedence_TenantOverrideValidated` |
| TLS config enforced | REQ-SEC-007 | `TestTLS_ConfigEnforced`, `TestTLS_InsecureRejectedInProd` |
| Secrets from env only | REQ-SEC-008 | `TestConfig_SecretsFromEnvOnly` |

### 2.14 `internal/observability`

Full matrix in `spec/observability.md` §10: `TestMetrics_InstrumentInventory`,
`TestMetrics_NoHighCardinalityAttributes`, `TestMetrics_TTLHitMissSemantics`,
`TestMetrics_ExemplarsAttached`, `TestMetrics_TenantCardinalityCollapse`,
`TestTracing_SpanTreeShape`, `TestTracing_ErrorRecording`,
`TestTracing_SamplerConfiguration`, `TestLogging_RequestLineSchema`,
`TestObservability_ExporterFailureIsolated`.

### 2.15 `pkg/errs` and `pkg/correlation`

| Behaviour | Req | Test |
|---|---|---|
| Code set closed and matches contract + OpenAPI | REQ-API-009 | `TestErrorCodes_ClosedSet` |
| `type` URI derivation | REQ-API-009 | `TestErrorType_URIDerivation` |
| Three-valued classification table | REQ-RES-004 | `TestErrs_ClassificationTable` |
| Correlation round-trip | REQ-API-010 | `TestCorrelation_RoundTrip` |
| Malformed correlation id replaced | REQ-API-010 | `TestCorrelation_RejectsMalformed` |
| `pkg/**` imports nothing from `internal/**` | §17.2 of requirements | `TestPkg_NoInternalImports` |

---

## 3. Integration tests — `test/integration`

Run in-process: a real `net/http` server with the full middleware chain and
handler set, a real cache (miniredis for L2), and the reference stubs
(`cmd/opsource`, `cmd/exsource`) started as goroutines on ephemeral ports. Only
the clock is faked.

### 3.1 Lifecycle tests

| Test | Asserts |
|---|---|
| `TestLifecycle_AllTwentyThreeSteps` | every step in `spec/architecture.md` §5 executes in order, verified via the span tree |
| `TestLifecycle_SpanTreeMatchesContract` | REQ-OBS-003 shape end to end |
| `TestLifecycle_MetaCompleteness` | every 2xx carries every mandatory `meta` field (REQ-API-004) |
| `TestLifecycle_DeterministicResponses` | two identical requests produce byte-identical bodies (REQ-API-012) |

### 3.2 The twenty edge cases

Each maps to a named test file; all are driven through the public HTTP surface.

| Edge | Test file | Tests |
|---|---|---|
| 001 fresh | `edge_freshness_test.go` | `TestEdge001_OperationalFresh` — additionally asserts the EDS fake recorded **zero** calls |
| 002 stale | `edge_freshness_test.go` | `TestEdge002_OperationalStale_FallsBack`, `..._ServesStale`, `TestEdge002_BeyondMaxStale` |
| 003 ODS unavailable | `edge_health_test.go` | `TestEdge003_OperationalUnavailable_Fallback`, `..._NoFallback` |
| 004 EDS unavailable | `edge_health_test.go` | `TestEdge004_ExecutionUnavailable_Required`, `..._Optional` |
| 005 both unavailable | `edge_health_test.go` | `TestEdge005_BothUnavailable_StaleServe`, `..._HardFail` |
| 006 ODS partial | `edge_partial_test.go` | `TestEdge006_OperationalPartial` |
| 007 EDS partial | `edge_partial_test.go` | `TestEdge007_ExecutionPartial` |
| 008 conflict | `edge_conflict_test.go` | `TestEdge008_ConflictResolution` |
| 009 timestamps | `edge_timestamp_test.go` | `TestEdge009_OldestObservationWins` |
| 010 clock skew | `edge_skew_test.go` | `TestEdge010_SkewDoesNotFakeFreshness`, `TestEdge010_NegativeAgeClamped` |
| 011 cache stale | `edge_cache_test.go` | `TestEdge011_CacheStaleReevaluated` |
| 012 concurrency | `edge_concurrency_test.go` | `TestEdge012_ConcurrentSingleflight`, `TestEdge012_WaiterCancelDoesNotAbortLeader` |
| 013 timeout | `edge_timeout_test.go` | `TestEdge013_SourceTimeoutOptional`, `..._Required` |
| 014 network | `edge_network_test.go` | `TestEdge014_NetworkFailureClasses`, `TestEdge014_NoPartialDecodeOnReset` |
| 015 running | `edge_running_test.go` | `TestEdge015_RunningExecutionOverride`, `TestEdge015_CacheTTLClamped` |
| 016 tenancy | `edge_tenant_test.go` | `TestEdge016_TenantIsolationCached`, `..._Uncached`, `TestEdge016_SourceReturnsForeignTenant` |
| 017 schema | `edge_schema_test.go` | `TestEdge017_MinorDriftTolerated`, `..._MajorMismatchFatal`, `..._MismatchAllowsFallback` |
| 018 partial response | `edge_partial_test.go` | `TestEdge018_PartialResponseShape` |
| 019 empty | `edge_empty_test.go` | `TestEdge019_NotFound`, `TestEdge019_EmptyCollectionIs200`, `TestEdge019_EmptyBodyIsInvalid` |
| 020 invalid | `edge_invalid_test.go` | `TestEdge020_InvalidPayloadClasses`, `..._NotRetried`, `..._OptionalSourceContinues` |

Every edge test asserts four things, not one: the HTTP status, the
`meta.routingRule`, the `degraded`/`partial` flags with their warnings, and the
metric deltas. Asserting the rule id is what makes these tests detect a *changed
reason* for a correct-looking answer.

### 3.3 Other integration tests

| Test | Req |
|---|---|
| `TestPerf_ProbeCheaperThanRead` (`probe_budget_test.go`) | REQ-PERF-002 |
| `TestIntegration_TenantOverlayAppliesEndToEnd` | REQ-MT-002, REQ-TTL-003 |
| `TestIntegration_HotReloadChangesTTLWithoutRestart` | REQ-CFG-004 |
| `TestIntegration_GracefulShutdownDrains` | REQ-RES-010 |
| `TestIntegration_L2DownDegradesToL1` | REQ-CACHE-001 |
| `TestIntegration_RBACMatrixEndToEnd` | REQ-SEC-005 |

---

## 4. Contract tests — `test/contract`

Verify that each source honours the properties the BFF depends on. Run against
the reference stubs on every commit, and against staging sources in a scheduled
pipeline. A failure against staging is a source-team defect, not a BFF one — the
report names which obligation broke.

### 4.1 ODS — `opsource_test.go`

`TestContract_OpSourceStub` plus the obligations enumerated in
`spec/operational-source.md` §8.1:

- all five RPCs present and callable;
- **probe P95 ≤ 15 ms and < 40% of `GetResource` P95 over ≥ 500 samples**
  (REQ-PERF-002) — the economic premise of the design, asserted numerically;
- probe returns no resource body;
- `last_updated` and `server_time` both populated on every read;
- `server_time` tracks the source clock (two calls 1 s apart differ by ≈ 1 s);
- `version` never decreases for a record;
- every `ResourceState` member producible and mapping per the table;
- `schema_version` present and semver-parseable;
- responses ≤ 4 MiB;
- `field_mask` honoured, `freshness` always returned;
- a foreign `tenant_id` never yields another tenant's record.

### 4.2 EDS — `exsource_test.go`

`TestContract_ExSourceStub` plus `spec/execution-source.md` §10.1:

- exactly the four endpoints;
- mandatory fields always present;
- every status spelling in the synonym table producible;
- ordering newest-first; the BFF's defensive re-sort is a no-op;
- cursor round-trips, last page omits it;
- empty collection is `200` with an empty `items` array, never `404`;
- `latest-execution` on a never-executed resource is `404`;
- timestamps RFC 3339 with `Z`; `completedAt` null iff non-terminal;
- responses ≤ 4 MiB at `limit=200`;
- **EDS P95 ≥ 3× ODS P95** over ≥ 500 samples — if this ever fails, the routing
  defaults and the argument in `spec/execution-source.md` §7 need revisiting.

### 4.3 API contract

| Test | Asserts |
|---|---|
| `TestContract_OpenAPIMatchesRoutes` | every route in the router appears in `api/openapi/bff-v1.yaml` and vice versa |
| `TestContract_OpenAPIErrorCodesMatchErrs` | the `ErrorCode` enum equals the `pkg/errs` constant set |
| `TestContract_OpenAPIRuleIdsMatchRouter` | the `RoutingRule` enum equals the router's rule id set |
| `TestContract_ResponsesValidateAgainstSchema` | every integration-test response body validates against the OpenAPI schema |
| `TestContract_SpecYAMLCopiesIdentical` | `spec/api-contract.yaml` and `api/openapi/bff-v1.yaml` are byte-identical |

The last one exists because two copies of a contract diverge silently otherwise.

---

## 5. Failure and chaos tests

### 5.1 Chaos knobs

Both reference sources expose an admin HTTP surface — `:9111` (opsource),
`:9112` (exsource) — with `PUT /chaos` taking a JSON document, `GET /chaos`
reporting the current values and `DELETE /chaos` resetting to clean behaviour.
Each also serves `GET /healthz`, `GET /livez` and `GET /readyz`, the same three
probes the BFF serves, which is what the compose health-checks target.

`PUT /chaos` accepts **any subset** of the knobs, so a test can change one
dimension without restating the rest.

**ODS knobs (`:9111`)**

```jsonc
{
  "latency_min_ms": 0,             // injected latency is sampled uniformly
  "latency_max_ms": 250,           //   between min and max...
                                   // ...and the freshness probe is delayed by a
                                   // TENTH of the sample, so the probe stays
                                   // materially cheaper than a read even under
                                   // injected latency
  "failure_rate": 0.3,             // fraction of calls answered UNAVAILABLE
  "unavailable": false,            // total outage; Health reports NOT_SERVING
  "probe_unavailable": false,      // fails ONLY GetResourceFreshness, so the
                                   // verdict is UNKNOWN while reads still work
  "stale_by_seconds": 120,         // added to every record's age
  "clock_skew_seconds": 10,        // shifts the reported server_time
  "partial": false,                // drop ownership/metrics/topology/metadata/labels
  "schema_version": "ods.v1"       // override the declared contract version
}
```

Record state is separate from chaos state and is **not** restored by
`DELETE /chaos`:

```
POST /resources/{id}/age?seconds=N             add N seconds to that record's age offset
POST /resources/{id}/touch                     zero the offset, bump freshness.version
POST /resources/{id}/in-flight?executionId=E   set in_flight_execution_ref
GET  /resources                                list the seeded ids
```

Ages are **offsets**, not timestamps: `R00i` is permanently `(i mod 7) * 5`
seconds old, recomputed on every read, so the seeded freshness spread does not
drift while the stack is up and a suite run at lunchtime behaves like one run at
nine. Restore the seeded spread by restarting `opsource`.

**EDS knobs (`:9112`)**

```jsonc
{
  "base_latency_ms": 120,          // default; the EDS really is the slow source
  "jitter_ms": 60,                 // default
  "failure_rate": 0.2,             // fraction of calls answered 502
  "unavailable": false,            // every call answered 503
  "malformed": false,              // drop executionId, which the mapper must reject
  "schema_version": "eds.v1"       // override the declared contract version
}
```

```
POST /resources/{resourceId}/executions?operation=&state=&resultingState=&tenantId=
```

inserts a new execution at the head of a resource's list, defaulting to
`state=IN_PROGRESS`. That is how a test puts the system into the state where the
`execution_overrides_when_running` precedence rule fires.

### 5.2 How the knobs are driven

`internal/testutil` provides a typed client so tests never hand-roll JSON:

```go
ops := testutil.NewChaos(opsAdminAddr)
defer ops.Reset(t)                       // t.Cleanup-registered; every test starts clean

ops.Apply(t, testutil.OpsChaos{
    StalenessOffset: 120 * time.Second,
    ClockOffset:     10 * time.Second,
})
```

Rules for chaos-driven tests:

1. **`Reset` is always deferred**, so no test inherits another's chaos. Enforced
   by a shared harness constructor rather than by convention.
2. **Knobs are applied before the request, never during**, unless the test is
   explicitly about mid-flight transitions (breaker opening under load), in which
   case a barrier synchronises the change.
3. **Assertions cover status, rule id, flags/warnings, and metric deltas.** A test
   asserting only the status code would pass for the right answer reached for the
   wrong reason.
4. **Chaos tests run with a faked clock** where staleness or TTL expiry is
   involved, so they are deterministic and fast.
5. `error_rate` is seeded deterministically per test so failure patterns
   reproduce.

### 5.3 Chaos scenarios

| Scenario | Knobs | Expectation | Req |
|---|---|---|---|
| ODS brownout | `failure_rate: 0.5` | retries bounded; breaker opens; fallback engages; no retry storm | REQ-RES-002/005 |
| ODS slow | `latency_max_ms: 900` (> `call_timeout`) | timeout, ladder, `504` or degraded | REQ-EDGE-013 |
| ODS probe fails, reads fine | `probe_unavailable: true` | verdict `UNKNOWN`; rule 10 applies `on_unknown_freshness`; reads still fast | REQ-TTL-005 |
| ODS clock fast | `clock_skew_seconds: 10` | single-domain age unaffected; `CLOCK_SKEW_DETECTED` | REQ-EDGE-010 |
| ODS clock grossly wrong | `clock_skew_seconds: 600` | verdict `UNKNOWN` | REQ-TTL-004 |
| ODS stale | `stale_by_seconds: 120`, or `POST /resources/{id}/age?seconds=120` for one record | rule `ttl.operational.stale`; branch per config | REQ-EDGE-002 |
| ODS refreshed again | `POST /resources/{id}/touch` | the route snaps back to `ttl.operational.fresh` | REQ-EDGE-001 |
| ODS major schema bump | `schema_version: "ods.v2"` | `SCHEMA_VERSION_MISMATCH`; **breaker untouched**; the call-time fallback serves where one is configured, else `502` | REQ-EDGE-017 |
| ODS partial record | `partial: true` | optional groups absent, not zero-filled; no false `partial` when not required | REQ-EDGE-006 |
| ODS in-flight | `POST /resources/{id}/in-flight?executionId=E` | precedence override on `status`/`subState`; `/status` consults the EDS via `resolve_in_flight_execution` | REQ-PREC-003 |
| ODS total outage | `unavailable: true` | first requests `fallback.primary_failed`, then `health.primary_unavailable` once the breaker opens; both `degraded: true` + `SOURCE_UNAVAILABLE` | REQ-EDGE-003 |
| EDS slow (optional branch) | `base_latency_ms: 2500` (> `per_source_timeout.execution`) | `206 partial` + `SOURCE_TIMEOUT`; the ODS branch is unaffected and never cancelled | REQ-AGG-002 |
| EDS malformed | `malformed: true` | `UPSTREAM_INVALID_RESPONSE`, not retried | REQ-EDGE-020 |
| EDS empty list | a resource with no executions | `200` with `items: []`, `total: 0`, no warning, not `404` | REQ-EDGE-019 |
| EDS running execution | `POST /resources/{id}/executions?state=IN_PROGRESS` | precedence resolves; conflict counted; provenance names the winner | REQ-EDGE-008 |
| Both down | both `unavailable: true` | rule 2; `degrade.stale_cache` or `503` | REQ-EDGE-005 |
| Redis down | miniredis stopped | L1-only; no request fails | REQ-CACHE-001 |
| Redis slow | proxy with injected latency | L2 lookup bounded; falls through to source | REQ-CACHE-001 |
| Client disconnect mid-request | harness cancels | upstream cancelled; breaker unaffected | REQ-RES-011 |
| Config reload storm | 100 reloads under load | requests see one consistent snapshot each | REQ-CFG-004 |
| Panic injection | fault-injecting mapper | `500`, process alive, counted | REQ-RES-012 |
| Goroutine leak sweep | all of the above under `goleak` | no leaks | REQ-AGG-005 |

---

## 6. Load tests — `test/load`

k6 scripts under `test/load/k6/`, run against a deployed BFF with the reference
sources configured to production-like latency profiles (ODS P95 45 ms, EDS P95
240 ms) via their chaos knobs.

### 6.1 Scenarios

| # | Script | Shape | Purpose |
|---|---|---|---|
| 1 | `baseline.js` | 200 RPS, 10 min, mixed endpoint weights (`status` 45%, `read` 25%, `details` 15%, `executions` 10%, `execution_status` 5%) | establish SLO compliance under nominal load (REQ-PERF-001) |
| 2 | `ttl_effectiveness.js` | 200 RPS against a **hot key set** of 50 resources, then a **cold set** of 50 000 | measure TTL hit ratio and cache hit ratio at both extremes; verify the fallback ratio moves as predicted |
| 3 | `stale_source.js` | 200 RPS with `staleness_offset_s: 60` applied at t+5 min | verify the fallback ratio rises, the EDS absorbs it, and P95 shifts to the EDS profile without errors |
| 4 | `fanout.js` | 100% `/details`, ramp 50 → 400 RPS | verify fan-out latency ≈ max(branches), and that EDS bulkhead saturation degrades to `partial` rather than to errors (REQ-PERF-006) |
| 5 | `saturation.js` | ramp to 2× bulkhead capacity | verify load shedding is bounded: `429`/`503` rise, P99 stays < 2× SLO, queue depth does not grow without bound (REQ-PERF-005) |
| 6 | `soak.js` | 150 RPS, 4 h | no memory growth, no goroutine growth, no connection-pool exhaustion, no L1 unbounded growth |

### 6.2 Thresholds (pass/fail)

```javascript
// baseline.js
export const options = {
  scenarios: { /* ... */ },
  thresholds: {
    // availability (REQ-PERF-001, spec/observability.md §7.1)
    'http_req_failed{expected_response:true}': ['rate<0.001'],
    'checks{check:envelope_complete}':         ['rate>0.999'],

    // latency per route class
    'http_req_duration{route:status}':     ['p(95)<60',  'p(99)<120'],
    'http_req_duration{route:read}':       ['p(95)<60',  'p(99)<120'],
    'http_req_duration{route:executions}': ['p(95)<250', 'p(99)<500'],
    'http_req_duration{route:details}':    ['p(95)<280', 'p(99)<550'],
    'http_req_duration{cache:hit}':        ['p(95)<5',   'p(99)<15'],

    // policy effectiveness — the tests unique to this service
    'ttl_hit_ratio':          ['value>0.90'],
    'execution_fallback_ratio':['value<0.05'],
    'stale_response_ratio':   ['value<0.01'],
    'partial_response_ratio': ['value<0.02'],
    'data_freshness_age_p95': ['value<60'],     // 2 × the 30s TTL

    // BFF overhead (REQ-PERF-003)
    'bff_overhead_ms{route:details}': ['p(99)<12'],
    'bff_overhead_ms{route:status}':  ['p(99)<6'],
  },
};
```

Scenario-specific overrides:

| Scenario | Threshold change | Rationale |
|---|---|---|
| `ttl_effectiveness` (cold set) | `ttl_hit_ratio > 0.85`, `cache_hit_ratio > 0.10` | a cold key set legitimately has a low cache hit ratio; the TTL policy must still work |
| `stale_source` | `execution_fallback_ratio` expected **> 0.80**; `http_req_failed < 0.001` | the point is that fallback engages *without* errors |
| `fanout` | `p(99) < 550`, `partial_response_ratio < 0.10` at 400 RPS | saturation degrades to partial, not to errors |
| `saturation` | `p(99) < 1100` (2× SLO); `429`+`503` combined < 30%; **zero** `500` | shedding must be graceful and never internal-error |
| `soak` | RSS growth < 5% over 4 h; goroutine count stable ±10%; no threshold regression vs. hour 1 | leak detection |

Custom k6 metrics (`ttl_hit_ratio`, `execution_fallback_ratio`,
`data_freshness_age_p95`, `bff_overhead_ms`) are computed in the script from
response `meta` — `meta.routingRule`, `meta.freshness`, `meta.degraded`,
`meta.partial` and the `X-Routing-Rule` header — so the load test verifies the
policy, not just the transport. This is only possible because the envelope
carries the decision (REQ-API-004, REQ-RT-009); a BFF that did not expose its
routing rule could not be load-tested for policy correctness.

### 6.3 Load-test harness assertions

`test/load/thresholds_test.go` (`TestPerf_LatencyBudgets`,
`TestPerf_BFFOverhead`) parses the k6 JSON summary and fails the build on a
threshold breach, so load results gate a release rather than being advisory.
`test/load/saturation_test.go` (`TestPerf_SaturationShedsLoad`) asserts the
shedding shape.

---

## 7. Coverage targets

| Scope | Statement coverage | Branch/table completeness |
|---|---|---|
| `internal/router` | **≥ 95%** | every rule × every truth-table row |
| `internal/freshness` | **≥ 95%** | every verdict × skew combination |
| `internal/policy` | **≥ 95%** | every field group × conflict case |
| `internal/mapper` | **≥ 95%** | every field, every enum member, every drop-list entry |
| `pkg/errs` | **≥ 95%** | every code × classification |
| `internal/aggregation` | ≥ 90% | full partial matrix |
| `internal/cache` | ≥ 90% | every layer × hit/miss/stale combination |
| `internal/resilience` | ≥ 90% | every retry-eligibility row, every breaker transition |
| `internal/security` | ≥ 90% | full JWT and RBAC matrices |
| `internal/config` | ≥ 90% | every validation rule |
| `internal/api/**` | ≥ 85% | every status-mapping row |
| `internal/datasource/**` | ≥ 80% | every error-translation row |
| `internal/observability` | ≥ 80% | instrument inventory, attribute allow-list |
| Module total | **≥ 85%** | — |
| `internal/domain` | n/a | structural tests only |

The five 95% packages are the ones where a bug is a *silent wrong answer* rather
than a visible failure — routing, freshness, precedence, mapping and error
classification. Everywhere else, a defect surfaces as an error the observability
stack reports.

Coverage is a floor, not a goal. The binding constraints are the completeness
assertions: `TestRouter_TruthTable`, `TestMapper_CompletenessAgainstDropList`,
`TestErrs_ClassificationTable`, `TestRetry_EligibilityTable`,
`TestStatusMapping_Table`, `TestMetrics_InstrumentInventory`,
`TestRBAC_EndpointPermissionMatrix`. Each fails when a table gains or loses a row
without the spec being updated — they are the mechanism that keeps this
documentation and the code from drifting.

---

## 8. CI pipeline

| Stage | Runs | Gate |
|---|---|---|
| 1 · lint | `gofmt`, `go vet`, `staticcheck`, `golangci-lint` | blocking |
| 2 · generate check | `scripts/gen-proto.sh` produces no diff | blocking |
| 3 · unit | `go test -race -count=1 ./internal/... ./pkg/...` | blocking |
| 4 · coverage | per-package floors from §7 | blocking |
| 5 · contract | `go test -race ./test/contract/...` | blocking |
| 6 · integration | `go test -race ./test/integration/...` | blocking |
| 7 · chaos | integration suite with chaos scenarios enabled | blocking |
| 8 · leak sweep | `goleak` enabled across all suites | blocking |
| 9 · spec sync | OpenAPI ↔ router ↔ `pkg/errs` ↔ rule ids; `spec/api-contract.yaml` and `api/openapi/bff-v1.yaml` byte-identical | blocking |
| 10 · bench | `go test -bench . -benchmem`, compared against the stored baseline | warn on > 10% regression, block on > 25% |
| 11 · load (nightly) | k6 scenarios 1–5 | blocking on the nightly branch |
| 12 · soak (pre-release) | k6 scenario 6, 4 h | blocking for a release tag |
| 13 · staging contract (scheduled) | contract suite against real sources | non-blocking; opens an issue against the source team |

`-race` is on for every Go stage. The concurrency requirements (REQ-AGG-001..005,
REQ-CACHE-004, REQ-CFG-004) are exactly the class of defect a non-race build
hides.

---

## 9. Test infrastructure — `internal/testutil`

| Helper | Purpose |
|---|---|
| `FakeClock` | injected everywhere a duration or age is computed; `Advance(d)` for deterministic TTL expiry |
| `NewChaos(addr)` | typed chaos client for both stubs, with `t.Cleanup`-registered `Reset` |
| `FakeOperationalPort`, `FakeExecutionPort` | port-level fakes for unit tests; record call counts so `TestEdge001` can assert **zero** EDS calls |
| `Harness` | in-process server + stubs + cache + config, one constructor, cleanup registered |
| `AssertEnvelope(t, resp, want)` | structural assertion over status, rule id, flags, warnings and provenance in one call |
| `MetricSnapshot` / `AssertDelta` | metric-delta assertions so tests verify observability, not just behaviour |
| `SpanRecorder` | in-memory span exporter for span-tree assertions |
| `MintJWT(claims...)` | signs test tokens with a test JWKS, including deliberately malformed ones |
| `GoldenJSON(t, name, got)` | golden-file compare with `-update` support |
| `Seeded(t)` | deterministic RNG per test, so jitter and chaos error rates reproduce |

`AssertEnvelope` exists so that the four-part assertion described in §3.2 is one
line rather than four, which is what makes it realistic to demand it in every
edge test.
