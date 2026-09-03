package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore/ttl-aware-bff/internal/policy"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

var now = time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)

// probeStub returns a fixed observation, or an error.
type probeStub struct {
	obs   datasource.Observation
	err   error
	calls int
}

func (p *probeStub) ProbeFreshness(context.Context, string, string) (datasource.Observation, error) {
	p.calls++
	if p.err != nil {
		return datasource.Observation{}, p.err
	}
	return p.obs, nil
}

// fixture builds a router over the default configuration with a programmable
// freshness probe.
func fixture(t *testing.T, probe *probeStub, mutate func(*config.Config)) (*Router, *config.Provider) {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	testutil.NoError(t, config.Validate(&cfg), "fixture configuration must be valid")

	provider := config.NewProvider(cfg)
	catalog := policy.NewFieldCatalog(cfg.Precedence)
	mgr := freshness.NewManager(probe, 0, freshness.NoopHooks()).WithClock(func() time.Time { return now })
	return New(provider, catalog, mgr, NoopHooks()), provider
}

func healthy() Health   { return Health{Available: true, Detail: "HEALTHY"} }
func unhealthy() Health { return Health{Available: false, Detail: "CIRCUIT_OPEN"} }

func fresh(age time.Duration) *probeStub {
	return &probeStub{obs: datasource.Observation{
		Found:       true,
		LastUpdated: now.Add(-age),
		SourceTime:  now,
	}}
}

func request(requestType string, opsHealth, execHealth Health, fields ...string) Request {
	if len(fields) == 0 {
		fields = policy.DefaultFieldsFor(requestType)
	}
	return Request{
		Classification: classifier.Classification{
			Type:           requestType,
			TenantID:       "t1",
			ResourceID:     "R1",
			RequiredFields: fields,
			Consistency:    classifier.ConsistencyBounded,
		},
		OperationalHealth: opsHealth,
		ExecutionHealth:   execHealth,
		Now:               now,
	}
}

// TestSelect_TTLHitAvoidsTheSlowSource verifies REQ-RT-008 and REQ-PERF-004:
// when the operational copy is within its TTL, the decision is OPERATIONAL and
// the execution source is not involved at all. This is the rule the entire
// design exists to make possible.
func TestSelect_TTLHitAvoidsTheSlowSource(t *testing.T) {
	t.Parallel()

	r, _ := fixture(t, fresh(3*time.Second), nil) // resource_status TTL is 10s
	d, err := r.Select(context.Background(), request("resource_status", healthy(), healthy()))

	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetOperational, "target")
	testutil.Equal(t, d.Rule, RuleTTLFresh, "rule id")
	testutil.False(t, d.Target.Includes(domain.SourceExecution), "the execution source is not consulted")
	testutil.Equal(t, d.Freshness.State, domain.FreshnessFresh, "reported freshness")
}

// TestSelect_TTLMissRoutesToFallback verifies REQ-RT-009: past the TTL, the
// request goes to the configured fallback source.
func TestSelect_TTLMissRoutesToFallback(t *testing.T) {
	t.Parallel()

	r, _ := fixture(t, fresh(45*time.Second), nil) // well past the 10s TTL
	d, err := r.Select(context.Background(), request("resource_status", healthy(), healthy()))

	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetExecution, "target")
	testutil.Equal(t, d.Rule, RuleTTLStale, "rule id")
	testutil.Equal(t, d.Freshness.State, domain.FreshnessStale, "reported freshness")
}

// TestSelect_TTLMissServesStaleWhenNoFallback verifies REQ-RT-009 and
// REQ-RES-007: with no usable fallback, a request type that permits stale data
// still gets an answer from the operational source rather than a failure.
func TestSelect_TTLMissServesStaleWhenNoFallback(t *testing.T) {
	t.Parallel()

	// resource_configuration has fallback: none, allow_stale: true, TTL 30s.
	r, _ := fixture(t, fresh(90*time.Second), nil)
	d, err := r.Select(context.Background(), request("resource_configuration", healthy(), healthy()))

	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetOperational, "the only source that has configuration still answers")
	testutil.True(t, d.AllowStale, "and the decision records that stale data was permitted")
}

// TestSelect_TTLMissRefusedPastCeiling verifies REQ-TTL-005: max_stale is a
// hard ceiling. Past it, stale data is refused even though allow_stale is set.
func TestSelect_TTLMissRefusedPastCeiling(t *testing.T) {
	t.Parallel()

	// resource_configuration: TTL 30s, max_stale 300s, no fallback.
	r, _ := fixture(t, fresh(10*time.Minute), nil)
	d, err := r.Select(context.Background(), request("resource_configuration", healthy(), healthy()))

	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetNone,
		"data ten minutes old is past the five-minute ceiling and must not be served")
	testutil.Equal(t, d.Rule, RuleTTLStale, "rule id")
}

// TestSelect_UnknownFreshnessAppliesPolicy verifies REQ-TTL-006 and
// REQ-EDGE-003: a failed probe is a routing input, not a request failure.
func TestSelect_UnknownFreshnessAppliesPolicy(t *testing.T) {
	t.Parallel()

	t.Run("configured default is used", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, &probeStub{err: errors.New("probe failed")}, nil)
		d, err := r.Select(context.Background(), request("resource_status", healthy(), healthy()))
		testutil.NoError(t, err, "a probe failure must not fail routing")
		testutil.Equal(t, d.Rule, RuleTTLUnknown, "rule id")
		testutil.Equal(t, d.Target, TargetOperational, "on_unknown_freshness defaults to operational")
		testutil.True(t, d.ProbeFailed, "the decision records that the probe failed")
	})

	t.Run("falls to the other source when the default is down", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, &probeStub{err: errors.New("probe failed")}, nil)
		d, err := r.Select(context.Background(), request("resource_status", unhealthy(), healthy()))
		testutil.NoError(t, err, "select")
		testutil.Equal(t, d.Target, TargetExecution, "the healthy source is chosen instead")
	})

	t.Run("configuration can prefer the execution source", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, &probeStub{err: errors.New("probe failed")}, func(c *config.Config) {
			c.Routing.Defaults.OnUnknownFreshness = "execution"
		})
		d, err := r.Select(context.Background(), request("resource_status", healthy(), healthy()))
		testutil.NoError(t, err, "select")
		testutil.Equal(t, d.Target, TargetExecution, "the configured default is honoured")
	})
}

// TestSelect_NoProbeWhenTTLIsZero verifies REQ-PERF-002: a request type that
// will not accept an age-based answer must not pay for a freshness probe.
func TestSelect_NoProbeWhenTTLIsZero(t *testing.T) {
	t.Parallel()

	p := fresh(time.Second)
	r, _ := fixture(t, p, nil)
	_, err := r.Select(context.Background(), request("execution_history", healthy(), healthy()))
	testutil.NoError(t, err, "select")
	testutil.Equal(t, p.calls, 0,
		"execution_history has ttl 0, so asking the operational source how old its copy is would be wasted work")
}

// TestSelect_FieldRequirementsDecideBeforeTTL verifies REQ-RT-003 and
// REQ-RT-004: when the requested fields can only come from one source, that
// settles the decision.
func TestSelect_FieldRequirementsDecideBeforeTTL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		fields []string
		want   Target
		rule   string
	}{
		{"execution-only fields", []string{policy.FieldExecutionHistory}, TargetExecution, RuleFieldsExecOnly},
		// Operational-only fields PIN the source rather than terminating the
		// chain, so the emitted rule is the TTL verdict. That is deliberate:
		// terminating here would skip the max_stale ceiling for a request type
		// that has nowhere else to go, which is exactly where the ceiling
		// matters most.
		{"operational-only fields", []string{policy.FieldConfiguration, policy.FieldMetrics}, TargetOperational, RuleTTLFresh},
		{"fields spanning both", []string{policy.FieldConfiguration, policy.FieldLatestExecution}, TargetBoth, RuleFieldsSpanBoth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := fixture(t, fresh(time.Second), nil)
			d, err := r.Select(context.Background(), request("resource_read", healthy(), healthy(), tc.fields...))
			testutil.NoError(t, err, "select")
			testutil.Equal(t, d.Target, tc.want, "target")
			testutil.Equal(t, d.Rule, tc.rule, "rule id")
		})
	}
}

// TestSelect_SpanBothDegradesToTheHealthySide verifies REQ-EDGE-004: a
// both-source request with one side down is served from the other rather than
// being issued as a call that is certain to fail.
func TestSelect_SpanBothDegradesToTheHealthySide(t *testing.T) {
	t.Parallel()

	fields := []string{policy.FieldConfiguration, policy.FieldLatestExecution}

	r, _ := fixture(t, fresh(time.Second), nil)
	d, err := r.Select(context.Background(), request("resource_read", healthy(), unhealthy(), fields...))
	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetOperational, "the operational side still answers")
	testutil.Equal(t, d.Rule, RuleFieldsSpanBoth, "rule id")

	d, err = r.Select(context.Background(), request("resource_read", unhealthy(), healthy(), fields...))
	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetExecution, "or the execution side")
}

// TestSelect_StrongConsistencySkipsTTL verifies REQ-RT-006 and REQ-CLS-005: a
// strongly consistent request reads the authoritative source live and never
// accepts stale data.
func TestSelect_StrongConsistencySkipsTTL(t *testing.T) {
	t.Parallel()

	p := fresh(10 * time.Minute) // stale enough that the TTL rules would divert
	r, _ := fixture(t, p, nil)

	req := request("resource_status", healthy(), healthy())
	req.Classification.Consistency = classifier.ConsistencyStrong

	d, err := r.Select(context.Background(), req)
	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Rule, RuleStrongConsistency, "rule id")
	testutil.Equal(t, d.Target, TargetOperational, "the preferred source is read live")
	testutil.False(t, d.AllowStale, "and stale data is forbidden regardless of configuration")
	testutil.Equal(t, p.calls, 0, "no probe is needed: the answer must be live either way")
}

// TestSelect_HealthGates verifies REQ-RT-002 and REQ-RT-007.
func TestSelect_HealthGates(t *testing.T) {
	t.Parallel()

	t.Run("preferred source down uses the fallback", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, fresh(time.Second), nil)
		d, err := r.Select(context.Background(), request("resource_status", unhealthy(), healthy()))
		testutil.NoError(t, err, "select")
		testutil.Equal(t, d.Rule, RulePrimaryUnavailable, "rule id")
		testutil.Equal(t, d.Target, TargetExecution, "target")
	})

	t.Run("both sources down yields NONE", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, fresh(time.Second), nil)
		d, err := r.Select(context.Background(), request("resource_status", unhealthy(), unhealthy()))
		testutil.NoError(t, err, "select")
		testutil.Equal(t, d.Rule, RuleBothUnavailable, "rule id")
		testutil.Equal(t, d.Target, TargetNone, "target")
		testutil.True(t, len(d.Reason) > 0, "the reason names both sources' states")
	})

	t.Run("execution-only fields with the execution source down yields NONE", func(t *testing.T) {
		t.Parallel()
		r, _ := fixture(t, fresh(time.Second), nil)
		d, err := r.Select(context.Background(),
			request("execution_history", healthy(), unhealthy(), policy.FieldExecutionHistory))
		testutil.NoError(t, err, "select")
		testutil.Equal(t, d.Target, TargetNone, "there is no other source that holds execution history")
	})
}

// TestSelect_MissingTenantIsRefused verifies REQ-MT-001: a request that reached
// the router without a resolved tenant is refused rather than routed, so a bug
// in the auth middleware can never become a cross-tenant read.
func TestSelect_MissingTenantIsRefused(t *testing.T) {
	t.Parallel()

	r, _ := fixture(t, fresh(time.Second), nil)
	req := request("resource_status", healthy(), healthy())
	req.Classification.TenantID = "   "

	d, err := r.Select(context.Background(), req)
	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetNone, "target")
	testutil.Equal(t, d.Rule, RuleTenantMissing, "rule id")
}

// TestSelect_TenantOverrideChangesTheDecision verifies REQ-MT-005 and
// REQ-CFG-005: the same resource, at the same age, routes differently for two
// tenants because their configured TTLs differ. This is the end-to-end proof
// that the TTL is data rather than code.
func TestSelect_TenantOverrideChangesTheDecision(t *testing.T) {
	t.Parallel()

	withTenants := func(c *config.Config) {
		five, twenty := config.Duration(5*time.Second), config.Duration(20*time.Second)
		c.Tenants = map[string]config.Override{
			"impatient": {Routing: &config.RoutingConfig{RequestTypes: map[string]config.RoutingRule{
				"resource_status": {TTL: five, CacheTTL: config.Duration(time.Second), MaxStale: config.Duration(time.Minute), AllowStale: true},
			}}},
			"relaxed": {Routing: &config.RoutingConfig{RequestTypes: map[string]config.RoutingRule{
				"resource_status": {TTL: twenty, CacheTTL: config.Duration(time.Second), MaxStale: config.Duration(time.Minute), AllowStale: true},
			}}},
		}
	}

	// One record, twelve seconds old.
	r, _ := fixture(t, fresh(12*time.Second), withTenants)

	req := request("resource_status", healthy(), healthy())
	req.Classification.TenantID = "impatient"
	d, err := r.Select(context.Background(), req)
	testutil.NoError(t, err, "select for the impatient tenant")
	testutil.Equal(t, d.Target, TargetExecution, "12s is past a 5s TTL")

	req.Classification.TenantID = "relaxed"
	d, err = r.Select(context.Background(), req)
	testutil.NoError(t, err, "select for the relaxed tenant")
	testutil.Equal(t, d.Target, TargetOperational, "the same 12s is within a 20s TTL")
}

// TestSelect_CarriesConfiguredBudgets verifies REQ-RT-011: per-source timeouts,
// required-source flags and both TTLs travel with the decision, so no
// downstream component has to re-read configuration.
func TestSelect_CarriesConfiguredBudgets(t *testing.T) {
	t.Parallel()

	r, _ := fixture(t, fresh(time.Second), nil)
	d, err := r.Select(context.Background(), request("resource_details", healthy(), healthy()))
	testutil.NoError(t, err, "select")

	testutil.Equal(t, d.Target, TargetBoth, "target")
	testutil.Equal(t, d.OperationalTTL, 30*time.Second, "source freshness TTL")
	testutil.Equal(t, d.CacheTTL, 5*time.Second, "response cache TTL, which is a different thing")
	testutil.True(t, d.RequiresSource(domain.SourceOperational), "the operational side is required")
	testutil.False(t, d.RequiresSource(domain.SourceExecution),
		"the execution side is optional, which is what makes a partial response possible")
	testutil.Equal(t, d.TimeoutFor(domain.SourceOperational), 400*time.Millisecond, "operational budget")
	testutil.Equal(t, d.TimeoutFor(domain.SourceExecution), 1500*time.Millisecond, "execution budget")
	testutil.Equal(t, d.Fallback, domain.SourceOperational,
		"the configured fallback travels with the decision for the call-time fallback path")
}

// TestSelect_UnknownRequestTypeYieldsNone verifies REQ-CFG-004: a request type
// with no routing rule is a deployment error and must not be guessed at.
func TestSelect_UnknownRequestTypeYieldsNone(t *testing.T) {
	t.Parallel()

	r, _ := fixture(t, fresh(time.Second), nil)
	d, err := r.Select(context.Background(), request("no_such_type", healthy(), healthy(), policy.FieldStatus))
	testutil.NoError(t, err, "select")
	testutil.Equal(t, d.Target, TargetNone, "target")
}

// TestSelect_ProbeIsMadeAtMostOncePerRequest verifies REQ-PERF-002: several
// rules consult freshness, but the source is asked once.
func TestSelect_ProbeIsMadeAtMostOncePerRequest(t *testing.T) {
	t.Parallel()

	p := fresh(45 * time.Second) // stale, so the fresh rule passes and the stale rule fires
	r, _ := fixture(t, p, nil)
	_, err := r.Select(context.Background(), request("resource_status", healthy(), healthy()))
	testutil.NoError(t, err, "select")
	testutil.Equal(t, p.calls, 1, "the freshness probe is memoised within one routing pass")
}

// TestTarget_Includes documents the target/source relationship the aggregator
// depends on.
func TestTarget_Includes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target Target
		ops    bool
		exec   bool
	}{
		{TargetNone, false, false},
		{TargetOperational, true, false},
		{TargetExecution, false, true},
		{TargetBoth, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.target.String(), func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, tc.target.Includes(domain.SourceOperational), tc.ops, "operational")
			testutil.Equal(t, tc.target.Includes(domain.SourceExecution), tc.exec, "execution")
		})
	}
}
