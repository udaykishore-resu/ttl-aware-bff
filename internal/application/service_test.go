package application_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/application"
	"github.com/udaykishore/ttl-aware-bff/internal/cache"
	"github.com/udaykishore/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore/ttl-aware-bff/internal/observability"
	"github.com/udaykishore/ttl-aware-bff/internal/policy"
	"github.com/udaykishore/ttl-aware-bff/internal/router"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

var now = time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)

const (
	tenant   = "acme"
	resource = "R-1"
)

// harness wires a real Service over fake sources, so every test exercises the
// genuine lifecycle -- classify, route, fetch, map, merge, apply precedence,
// build the envelope -- rather than a mock of it.
type harness struct {
	svc   *application.Service
	ops   *testutil.FakeOperational
	execs *testutil.FakeExecution
	clk   *testutil.Clock
	cfg   *config.Provider
	cls   *classifier.Classifier
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Cache.Backend = "memory"
	cfg.Cache.Enabled = true
	if mutate != nil {
		mutate(&cfg)
	}
	testutil.NoError(t, config.Validate(&cfg), "harness configuration must be valid")

	provider := config.NewProvider(cfg)
	clk := testutil.NewClock(now)
	ops := testutil.NewFakeOperational()
	execs := testutil.NewFakeExecution()

	catalog := policy.NewFieldCatalog(cfg.Precedence)
	prec := policy.NewPrecedence(cfg.Precedence, nil)
	fm := freshness.NewManager(ops, cfg.Routing.Defaults.ProbeCacheTTL.D(), freshness.NoopHooks()).
		WithClock(clk.Now)
	rt := router.New(provider, catalog, fm, router.NoopHooks())

	backend := cache.NewMemory(1000).WithClock(clk.Now)
	mgr := cache.NewManager(backend, cfg.Cache, cache.NoopHooks()).WithClock(clk.Now)

	svc := application.New(application.Deps{
		Config:      provider,
		Router:      rt,
		Operational: ops,
		Execution:   execs,
		Precedence:  prec,
		Catalog:     catalog,
		Cache:       mgr,
		Observer:    observability.NewNoopProvider(),
		Logger:      slog.New(slog.DiscardHandler),
	}).WithClock(clk.Now)

	return &harness{svc: svc, ops: ops, execs: execs, clk: clk, cfg: provider, cls: classifier.New(provider, catalog)}
}

// seedOperational installs a record and a matching freshness observation.
func (h *harness) seedOperational(age time.Duration, status domain.ResourceStatus) {
	observed := h.clk.Now().Add(-age)
	h.ops.Resources[resource] = &domain.Resource{
		TenantID:      tenant,
		ResourceID:    resource,
		CustomerID:    "C-1",
		Type:          "database",
		Status:        status,
		SubState:      "healthy",
		Configuration: map[string]string{"tier": "premium"},
		Owner:         &domain.Owner{ID: "team-1", Type: "team"},
		ObservedAt:    observed,
	}
	h.ops.Observations[resource] = datasource.Observation{
		Found:       true,
		LastUpdated: observed,
		SourceTime:  h.clk.Now(),
	}
}

func (h *harness) seedExecution(status domain.ExecutionStatus, after domain.ResourceStatus) *domain.Execution {
	e := &domain.Execution{
		ExecutionID:         "E-1",
		TenantID:            tenant,
		ResourceID:          resource,
		Operation:           "RESIZE",
		Status:              status,
		ResourceStatusAfter: after,
		StartedAt:           h.clk.Now().Add(-2 * time.Minute),
		UpdatedAt:           h.clk.Now().Add(-time.Minute),
	}
	h.execs.Latest[resource] = e
	h.execs.ByID[e.ExecutionID] = e
	h.execs.History[resource] = &domain.ExecutionList{ResourceID: resource, Items: []domain.Execution{*e}, Total: 1}
	return e
}

func (h *harness) classify(t *testing.T, route, method, resourceID, executionID string) classifier.Classification {
	t.Helper()
	cls, err := h.cls.Classify(classifier.Input{
		Route: route, Method: method,
		TenantID: tenant, ResourceID: resourceID, ExecutionID: executionID,
	})
	testutil.NoError(t, err, "classification")
	return cls
}

func decode[T any](t *testing.T, env *application.Envelope) T {
	t.Helper()
	raw, ok := env.Data.(json.RawMessage)
	testutil.True(t, ok, "payload is raw JSON")
	var out T
	testutil.NoError(t, json.Unmarshal(raw, &out), "decode payload")
	return out
}

// ---------------------------------------------------------------------------

// TestGetResourceStatus_FreshOperationalNeverTouchesExecution verifies
// REQ-PERF-004: the point of the whole design is that a fresh operational copy
// answers the request without the slow source being called at all.
func TestGetResourceStatus_FreshOperationalNeverTouchesExecution(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive) // TTL is 10s

	env, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.NoError(t, err, "request")

	testutil.Equal(t, env.Meta.RoutingDecision, "OPERATIONAL", "routing decision")
	testutil.Equal(t, env.Meta.RoutingRule, router.RuleTTLFresh, "routing rule")
	testutil.Equal(t, env.Meta.Freshness.State, domain.FreshnessFresh, "freshness")
	testutil.False(t, env.Meta.Degraded, "a fresh answer is not degraded")
	testutil.Equal(t, h.execs.CallCount("GetLatestExecution"), 0,
		"the execution source must not be called when operational data satisfies the TTL")

	body := decode[map[string]any](t, env)
	testutil.Equal(t, body["status"], any(string(domain.StatusActive)), "status")
}

// TestGetResourceStatus_StaleOperationalRoutesToExecution verifies REQ-RT-009
// end to end: past the TTL the request is served by the execution source.
func TestGetResourceStatus_StaleOperationalRoutesToExecution(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(90*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusSuspended)

	env, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.NoError(t, err, "request")

	testutil.Equal(t, env.Meta.RoutingRule, router.RuleTTLStale, "routing rule")
	testutil.Equal(t, env.Meta.RoutingDecision, "EXECUTION", "routing decision")
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 0,
		"the operational source is not read once its copy is known to be too old")
}

// TestCallTimeFallback verifies REQ-RES-006 and REQ-EDGE-003: the first
// failures of an outage arrive while the breaker still believes the source is
// healthy, so the fallback has to be applied at call time, not only at routing
// time.
func TestCallTimeFallback(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusSuspended)

	// The source still REPORTS itself healthy -- that is the whole point.
	h.ops.SetError("GetResourceState", errs.New(errs.CodeUpstreamUnavailable, "boom"))

	env, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.NoError(t, err, "the request must still be answered")

	testutil.Equal(t, env.Meta.RoutingRule, router.RuleFallbackAfterFailure, "routing rule")
	testutil.Equal(t, env.Meta.RoutingDecision, "EXECUTION", "routing decision")
	testutil.True(t, env.Meta.Degraded, "a fallback answer is degraded")
	testutil.True(t, hasWarning(env.Meta, domain.WarnSourceUnavailable), "and says so in a warning")
	testutil.Equal(t, h.execs.CallCount("GetLatestExecution"), 1, "the fallback source was read")
}

// TestCallTimeFallback_RefusedForClientErrors verifies REQ-RES-006: a fallback
// is for failures the other source might not share. A 404 is not one of them --
// retrying it elsewhere would turn a correct answer into a wrong one.
func TestCallTimeFallback_RefusedForClientErrors(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusActive)
	h.ops.SetError("GetResourceState", errs.ErrNotFound)

	_, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.ErrCode(t, err, errs.CodeNotFound, "a not-found must not be papered over by another source")
	testutil.Equal(t, h.execs.CallCount("GetLatestExecution"), 0, "and the fallback was not attempted")
}

// TestDetails_FanOutIsPartialWhenTheOptionalSourceFails verifies REQ-AGG-003
// and REQ-EDGE-004: the execution side of /details is optional, so its failure
// yields a partial answer rather than an error.
func TestDetails_FanOutIsPartialWhenTheOptionalSourceFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.execs.SetError("GetLatestExecution", errs.New(errs.CodeUpstreamTimeout, "slow"))
	h.execs.SetError("ListExecutions", errs.New(errs.CodeUpstreamTimeout, "slow"))

	env, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.NoError(t, err, "an optional source failing must not fail the request")

	testutil.True(t, env.Meta.Partial, "the response is marked partial")
	testutil.True(t, hasWarning(env.Meta, domain.WarnSourceTimeout), "and explains why")

	body := decode[map[string]any](t, env)
	testutil.Equal(t, body["status"], any(string(domain.StatusActive)), "the operational half is still present")
	_, hasExec := body["latestExecution"]
	testutil.False(t, hasExec, "and the execution half is simply absent")
}

// TestDetails_RequiredSourceFailureIsAnError verifies REQ-AGG-003: the
// operational side of /details is required, so its failure is not partial.
func TestDetails_RequiredSourceFailureIsAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusActive)
	h.ops.SetError("GetResource", errs.New(errs.CodeUpstreamUnavailable, "down"))

	_, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "a required source failing fails the request")
}

// TestPrecedence_RunningExecutionOverridesOperationalStatus verifies
// REQ-PREC-003 and REQ-EDGE-015: while a workflow is mutating the resource, the
// execution source's view of the nominated fields wins, and the response says
// which source supplied the value.
func TestPrecedence_RunningExecutionOverridesOperationalStatus(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecRunning, domain.StatusPending)

	env, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.NoError(t, err, "request")

	body := decode[map[string]any](t, env)
	testutil.Equal(t, body["status"], any(string(domain.StatusPending)),
		"the running execution's view of status wins")
	testutil.Equal(t, env.Meta.Provenance["status"], domain.SourceExecution, "and provenance records that")
	testutil.Equal(t, env.Meta.Provenance["configuration"], domain.SourceOperational,
		"while operational-only fields still come from the operational source")
}

// TestPrecedence_CompletedExecutionDoesNotOverride verifies REQ-PREC-003: the
// override is scoped to a RUNNING execution. A finished one has no claim on the
// resource's current state.
func TestPrecedence_CompletedExecutionDoesNotOverride(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusSuspended)

	env, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.NoError(t, err, "request")

	body := decode[map[string]any](t, env)
	testutil.Equal(t, body["status"], any(string(domain.StatusActive)),
		"the operational source observed the state; the execution source only predicted it")
	testutil.Equal(t, env.Meta.Provenance["status"], domain.SourceOperational, "provenance")
}

// TestInFlightResolution_KeepsStatusAndDetailsConsistent verifies REQ-PREC-003:
// an operational-only read consults the execution source when the operational
// record itself declares a workflow in flight, so the two endpoints cannot
// report different statuses for the same resource.
func TestInFlightResolution_KeepsStatusAndDetailsConsistent(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.ops.Resources[resource].InFlightExecutionID = "E-1"
	h.seedExecution(domain.ExecRunning, domain.StatusPending)

	statusEnv, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.NoError(t, err, "status request")

	detailsEnv, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.NoError(t, err, "details request")

	statusBody := decode[map[string]any](t, statusEnv)
	detailsBody := decode[map[string]any](t, detailsEnv)
	testutil.Equal(t, statusBody["status"], detailsBody["status"],
		"the two endpoints must agree about the same resource at the same instant")
	testutil.Equal(t, statusBody["status"], any(string(domain.StatusPending)), "and both reflect the running workflow")
}

// TestInFlightResolution_IsBestEffort verifies REQ-PREC-003: the extra lookup
// must never be able to fail a request that the operational source already
// answered.
func TestInFlightResolution_IsBestEffort(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(2*time.Second, domain.StatusActive)
	h.ops.Resources[resource].InFlightExecutionID = "E-1"
	h.execs.SetError("GetExecution", errs.New(errs.CodeUpstreamTimeout, "slow"))

	env, err := h.svc.GetResourceStatus(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, ""))
	testutil.NoError(t, err, "the operational answer must stand")

	body := decode[map[string]any](t, env)
	testutil.Equal(t, body["status"], any(string(domain.StatusActive)), "the operational value is served")
}

// TestCache_HitDoesNotLaunderStaleData verifies REQ-CACHE-001 and REQ-EDGE-011:
// an answer that sits in the cache keeps ageing, so a cache hit can and must
// report STALE.
func TestCache_HitDoesNotLaunderStaleData(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *config.Config) {
		// A long cache TTL against a short freshness TTL is exactly the
		// configuration where conflating the two would show.
		rule := c.Routing.RequestTypes["resource_status"]
		rule.TTL = config.Duration(10 * time.Second)
		rule.CacheTTL = config.Duration(10 * time.Second)
		c.Routing.RequestTypes["resource_status"] = rule
	})
	h.seedOperational(time.Second, domain.StatusActive)

	cls := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")

	first, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.NoError(t, err, "first request")
	testutil.Equal(t, first.Meta.Freshness.State, domain.FreshnessFresh, "the first answer is fresh")
	testutil.False(t, first.Meta.Cache.Hit, "and was not a cache hit")

	// Move past the freshness TTL but stay inside the cache TTL.
	h.clk.Advance(9500 * time.Millisecond)

	second, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.NoError(t, err, "second request")
	testutil.True(t, second.Meta.Cache.Hit, "the second answer came from the cache")
	testutil.Equal(t, second.Meta.Freshness.State, domain.FreshnessStale,
		"and reports the data as stale, because it is")
	testutil.True(t, second.Meta.Degraded, "a stale answer is degraded even when the cache hit was valid")
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 1, "the source was read only once")
}

// TestCache_ZeroTTLRequestTypesAreNotCached verifies REQ-CACHE-003: a request
// type configured with cache_ttl 0 reads live every time.
func TestCache_ZeroTTLRequestTypesAreNotCached(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedExecution(domain.ExecCompleted, domain.StatusActive)

	cls := h.classify(t, "/api/v1/resources/{resourceId}/executions", "GET", resource, "")
	for i := 0; i < 3; i++ {
		_, err := h.svc.ListExecutions(context.Background(), cls)
		testutil.NoError(t, err, "request %d", i)
	}
	testutil.Equal(t, h.execs.CallCount("ListExecutions"), 3,
		"execution history is configured cache_ttl 0s and must be read live every time")
}

// TestDegradation_StaleCacheServedWhenEverySourceIsDown verifies REQ-RES-007
// and REQ-EDGE-005: with nothing reachable, a clearly-labelled old answer beats
// a 503.
func TestDegradation_StaleCacheServedWhenEverySourceIsDown(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(time.Second, domain.StatusActive)
	cls := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")

	_, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.NoError(t, err, "warm the cache")

	// Everything goes away, and the cached answer expires.
	h.ops.SetHealth(false, "CIRCUIT_OPEN")
	h.execs.SetHealth(false, "CIRCUIT_OPEN")
	h.clk.Advance(5 * time.Second) // past the 3s cache TTL, inside max_stale

	env, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.NoError(t, err, "a stale answer must be served rather than a failure")
	testutil.True(t, env.Meta.Degraded, "and marked degraded")
	testutil.Equal(t, env.Meta.RoutingRule, "degrade.stale_cache", "with the degradation rule recorded")
	testutil.True(t, hasWarning(env.Meta, domain.WarnStaleData), "and a warning the UI can surface")
}

// TestDegradation_RefusedWhenPolicyForbidsStale verifies REQ-RES-007: serving
// stale data is a policy decision. A request type or tenant that forbids it
// gets an error, not an old answer.
func TestDegradation_RefusedWhenPolicyForbidsStale(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *config.Config) {
		rule := c.Routing.RequestTypes["resource_status"]
		rule.AllowStale = false
		rule.MaxStale = 0
		c.Routing.RequestTypes["resource_status"] = rule
		c.Routing.Defaults.AllowStale = config.Bool(false)
	})
	h.seedOperational(time.Second, domain.StatusActive)
	cls := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")

	_, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.NoError(t, err, "warm the cache")

	h.ops.SetHealth(false, "CIRCUIT_OPEN")
	h.execs.SetHealth(false, "CIRCUIT_OPEN")
	h.clk.Advance(5 * time.Second)

	_, err = h.svc.GetResourceStatus(context.Background(), cls)
	testutil.Error(t, err, "policy forbids stale data, so the request fails rather than serving it")
}

// TestNotFound_IsNegativelyCached verifies REQ-CACHE-007 and REQ-EDGE-019: a
// missing resource is a 404, and repeating the question does not repeatedly
// bother the sources.
func TestNotFound_IsNegativelyCached(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	cls := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")

	_, err := h.svc.GetResourceStatus(context.Background(), cls)
	testutil.ErrCode(t, err, errs.CodeNotFound, "first request")
	first := h.ops.CallCount("GetResourceState")

	_, err = h.svc.GetResourceStatus(context.Background(), cls)
	testutil.ErrCode(t, err, errs.CodeNotFound, "second request")
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), first,
		"the negative cache answered the repeat without touching the source")
}

// TestTenantIsolation_OneTenantCannotReadAnother verifies REQ-MT-004: the
// tenant travels with the read and the source's answer is checked against it.
func TestTenantIsolation_OneTenantCannotReadAnother(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(time.Second, domain.StatusActive)

	cls, err := h.cls.Classify(classifier.Input{
		Route:      "/api/v1/resources/{resourceId}/status",
		Method:     "GET",
		TenantID:   "someone-else",
		ResourceID: resource,
	})
	testutil.NoError(t, err, "classification")

	_, err = h.svc.GetResourceStatus(context.Background(), cls)
	testutil.ErrCode(t, err, errs.CodeNotFound,
		"another tenant's resource is not merely hidden from the response, it is not found at all")
}

// TestTenantIsolation_CacheIsNotShared verifies REQ-MT-003: one tenant warming
// the cache cannot serve another tenant's request.
func TestTenantIsolation_CacheIsNotShared(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(time.Second, domain.StatusActive)

	clsA := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")
	_, err := h.svc.GetResourceStatus(context.Background(), clsA)
	testutil.NoError(t, err, "tenant A warms the cache")
	warmed := h.ops.CallCount("GetResourceState")

	clsB := clsA
	clsB.TenantID = "other-tenant"
	_, err = h.svc.GetResourceStatus(context.Background(), clsB)
	testutil.ErrCode(t, err, errs.CodeNotFound, "tenant B does not get tenant A's cached answer")
	testutil.True(t, h.ops.CallCount("GetResourceState") > warmed,
		"and its request reached the source under its own tenant")
}

// TestExecution_ResourceScopedURLCannotReadAnotherResource verifies REQ-SEC-006:
// a resource-scoped execution URL is a scope, not decoration.
func TestExecution_ResourceScopedURLCannotReadAnotherResource(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	e := h.seedExecution(domain.ExecCompleted, domain.StatusActive)
	e.ResourceID = "some-other-resource"
	h.execs.ByID[e.ExecutionID] = e

	cls := h.classify(t, "/api/v1/resources/{resourceId}/executions/{executionId}", "GET", resource, "E-1")
	_, err := h.svc.GetExecution(context.Background(), cls)
	testutil.ErrCode(t, err, errs.CodeNotFound,
		"an execution belonging to another resource must not be readable through this resource's URL")
}

// TestConcurrentIdenticalRequests_CollapseToOneSourceCall verifies
// REQ-EDGE-012: a burst of identical misses must not become a burst of source
// calls.
func TestConcurrentIdenticalRequests_CollapseToOneSourceCall(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(time.Second, domain.StatusActive)
	h.ops.Delay = 20 * time.Millisecond

	cls := h.classify(t, "/api/v1/resources/{resourceId}/status", "GET", resource, "")

	const n = 40
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := h.svc.GetResourceStatus(context.Background(), cls)
			done <- err
		}()
	}
	for i := 0; i < n; i++ {
		testutil.NoError(t, <-done, "concurrent request %d", i)
	}

	testutil.True(t, h.ops.CallCount("GetResourceState") < n/2,
		"singleflight must collapse the burst; got %d source calls for %d requests",
		h.ops.CallCount("GetResourceState"), n)
}

// TestEnvelope_CarriesEverythingTheUINeeds verifies REQ-API-003: one response
// shape, always, with enough metadata that the UI never has to ask which source
// answered.
func TestEnvelope_CarriesEverythingTheUINeeds(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedOperational(3*time.Second, domain.StatusActive)
	h.seedExecution(domain.ExecCompleted, domain.StatusActive)

	env, err := h.svc.GetResourceDetails(context.Background(),
		h.classify(t, "/api/v1/resources/{resourceId}/details", "GET", resource, ""))
	testutil.NoError(t, err, "request")

	testutil.NotEqual(t, env.Meta.RoutingDecision, "", "routing decision is reported")
	testutil.NotEqual(t, env.Meta.RoutingRule, "", "routing rule is reported")
	testutil.True(t, len(env.Meta.Sources) > 0, "contributing sources are reported")
	testutil.True(t, len(env.Meta.Provenance) > 0, "per-field provenance is reported")
	testutil.Equal(t, env.Meta.Freshness.TTL, 30*time.Second, "the applied TTL is reported")
	testutil.True(t, env.Meta.Freshness.Age > 0, "and the data's age")

	// The freshness block must survive the wire format, including the fields
	// that are stored as durations internally.
	raw, err := json.Marshal(env.Meta.Freshness)
	testutil.NoError(t, err, "marshal freshness")
	var wire map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &wire), "unmarshal freshness")
	_, hasAge := wire["ageSeconds"]
	_, hasTTL := wire["ttlSeconds"]
	testutil.True(t, hasAge, "ageSeconds is on the wire")
	testutil.True(t, hasTTL, "ttlSeconds is on the wire")
}

func hasWarning(m domain.ResponseMeta, code string) bool {
	for _, w := range m.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
