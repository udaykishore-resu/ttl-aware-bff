package classifier

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// fixture builds a classifier over the shipped configuration, optionally
// mutated, so each test states the configuration it depends on.
func fixture(t *testing.T, mutate func(*config.Config)) *Classifier {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	return New(config.NewProvider(cfg), policy.NewFieldCatalog(cfg.Precedence))
}

// get builds a GET input for a route.
func get(route string) Input {
	return Input{Route: route, Method: "GET", TenantID: "acme", ResourceID: "R1"}
}

// TestClassify_RouteToRequestType verifies REQ-CLS-001: the request type comes
// from the matched route template and the method, never from the body, the
// user-agent, or a path value the caller controls. Every route the API contract
// exposes is asserted, because a route with no type is a request that cannot be
// routed at all.
func TestClassify_RouteToRequestType(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/api/v1/resources/{resourceId}":                          TypeResourceRead,
		"/api/v1/resources/{resourceId}/status":                   TypeResourceStatus,
		"/api/v1/resources/{resourceId}/configuration":            TypeResourceConfiguration,
		"/api/v1/resources/{resourceId}/details":                  TypeResourceDetails,
		"/api/v1/resources/{resourceId}/executions":               TypeExecutionHistory,
		"/api/v1/resources/{resourceId}/executions/{executionId}": TypeExecutionStatus,
	}
	for route, want := range cases {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			c := fixture(t, nil)
			got, err := c.Classify(get(route))
			testutil.NoError(t, err, "classify %s", route)
			testutil.Equal(t, got.Type, want, "request type for %s", route)
			testutil.Equal(t, got.TenantID, "acme", "the tenant is carried through")
			testutil.Equal(t, got.ResourceID, "R1", "as is the resource")
		})
	}

	t.Run("the route table covers exactly these routes", func(t *testing.T) {
		t.Parallel()
		// The table is fixed at construction because it mirrors the OpenAPI
		// contract: changing it is an API change, not a configuration change.
		testutil.Equal(t, len(fixture(t, nil).Routes()), len(cases), "route count")
	})
}

// TestClassify_UnrecognisedRouteIsAClientError verifies REQ-CLS-002: a route
// the classifier does not know is refused rather than guessed at. Guessing
// would make a typo in the API layer route to a real data source.
func TestClassify_UnrecognisedRouteIsAClientError(t *testing.T) {
	t.Parallel()

	c := fixture(t, nil)
	cases := map[string]Input{
		"unknown path": get("/api/v1/nope"),
		"empty route":  get(""),
		"known path, wrong method": {
			Route: "/api/v1/resources/{resourceId}", Method: "DELETE", TenantID: "acme", ResourceID: "R1",
		},
		"a raw path rather than a template": get("/api/v1/resources/R1/status"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := c.Classify(in)
			testutil.Error(t, err, "classification must fail")
			testutil.ErrCode(t, err, errs.CodeInvalidRequest, "an unroutable request is the caller's problem")
		})
	}
}

// TestClassify_UnknownFieldIsRejectedWithTheKnownOnes verifies REQ-CLS-003: a
// caller projection is validated against the field catalogue, and the failure
// carries the fields that would have worked. A bare "invalid field" leaves the
// caller guessing at a vocabulary they cannot see.
func TestClassify_UnknownFieldIsRejectedWithTheKnownOnes(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	c := fixture(t, nil)

	in := get("/api/v1/resources/{resourceId}")
	in.RequestedFields = []string{policy.FieldStatus, "lifecycleState"}

	_, err := c.Classify(in)
	testutil.Error(t, err, "an unmodelled field must not reach the router")
	testutil.ErrCode(t, err, errs.CodeInvalidRequest, "error code")

	e, ok := errs.As(err)
	testutil.True(t, ok, "the failure carries structured detail")
	testutil.Equal(t, e.Details["field"], any("lifecycleState"), "the offending field is named")

	known, ok := e.Details["known_fields"].([]string)
	testutil.True(t, ok, "the known fields travel with the error, got %#v", e.Details["known_fields"])
	testutil.Equal(t, known, policy.NewFieldCatalog(cfg.Precedence).KnownFields(),
		"and they are the catalogue's own sorted listing")
	testutil.True(t, len(known) > 0 && known[0] < known[len(known)-1],
		"presented in a stable order the caller can read")

	t.Run("a valid projection is accepted", func(t *testing.T) {
		t.Parallel()
		in := get("/api/v1/resources/{resourceId}")
		in.RequestedFields = []string{policy.FieldStatus, policy.FieldConfiguration}
		got, err := c.Classify(in)
		testutil.NoError(t, err, "classify")
		testutil.Equal(t, got.RequiredFields, []string{policy.FieldStatus, policy.FieldConfiguration},
			"the caller's projection is what the router is asked for")
	})
}

// TestClassify_ConsistencyOnlyTightens verifies REQ-CLS-004 and REQ-CLS-005: a
// caller may ask for more consistency than the endpoint promises, never for
// less. Honouring a request to relax would let any client opt out of the
// guarantee the endpoint was configured to make.
func TestClassify_ConsistencyOnlyTightens(t *testing.T) {
	t.Parallel()

	c := fixture(t, nil)
	cases := []struct {
		name      string
		route     string
		requested string
		want      Consistency
	}{
		// execution_status is configured strong.
		{"eventual cannot loosen strong", "/api/v1/resources/{resourceId}/executions/{executionId}", "eventual", ConsistencyStrong},
		{"bounded cannot loosen strong", "/api/v1/resources/{resourceId}/executions/{executionId}", "bounded", ConsistencyStrong},
		{"strong on strong", "/api/v1/resources/{resourceId}/executions/{executionId}", "strong", ConsistencyStrong},
		// resource_status is configured bounded.
		{"strong tightens bounded", "/api/v1/resources/{resourceId}/status", "strong", ConsistencyStrong},
		{"eventual cannot loosen bounded", "/api/v1/resources/{resourceId}/status", "eventual", ConsistencyBounded},
		{"no request keeps the configured level", "/api/v1/resources/{resourceId}/status", "", ConsistencyBounded},
		// resource_configuration is configured eventual.
		{"bounded tightens eventual", "/api/v1/resources/{resourceId}/configuration", "bounded", ConsistencyBounded},
		{"strong tightens eventual", "/api/v1/resources/{resourceId}/configuration", "strong", ConsistencyStrong},
		{"casing and padding are tolerated", "/api/v1/resources/{resourceId}/configuration", "  STRONG ", ConsistencyStrong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := get(tc.route)
			in.RequestedConsistency = tc.requested
			got, err := c.Classify(in)
			testutil.NoError(t, err, "classify")
			testutil.Equal(t, got.Consistency, tc.want, "consistency for %q on %s", tc.requested, tc.route)
		})
	}

	t.Run("an unrecognised token is refused", func(t *testing.T) {
		t.Parallel()
		in := get("/api/v1/resources/{resourceId}/status")
		in.RequestedConsistency = "immediate"
		_, err := c.Classify(in)
		testutil.ErrCode(t, err, errs.CodeInvalidRequest,
			"an unknown consistency token is refused rather than silently downgraded")
		testutil.True(t, strings.Contains(err.Error(), "strong"),
			"and the message names the accepted values, got %q", err.Error())
	})
}

// TestClassify_Limit verifies REQ-CLS-003 as it applies to pagination: the page
// size is configuration with a configured ceiling, so a caller cannot ask for a
// page large enough to hurt the execution source.
func TestClassify_Limit(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	pageSize := cfg.Sources.Execution.HistoryPageSize
	maxItems := cfg.Sources.Execution.MaxHistoryItems
	c := fixture(t, nil)

	cases := []struct {
		name      string
		requested int
		want      int
	}{
		{"unset falls back to the configured page size", 0, pageSize},
		{"negative falls back too", -5, pageSize},
		{"an explicit page inside the ceiling is honoured", 50, 50},
		{"exactly at the ceiling", maxItems, maxItems},
		{"above the ceiling is capped", maxItems * 10, maxItems},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := get("/api/v1/resources/{resourceId}/executions")
			in.Limit = tc.requested
			in.Cursor = "c-1"
			got, err := c.Classify(in)
			testutil.NoError(t, err, "classify")
			testutil.Equal(t, got.Limit, tc.want, "limit for a requested %d", tc.requested)
			testutil.Equal(t, got.Cursor, "c-1", "the cursor is carried through untouched")
		})
	}
}

// TestClassify_RequiredFields verifies REQ-CLS-003: the fields a request must
// produce come from the caller if they named any, then from the routing rule,
// then from the request type's declared defaults. The router turns that set
// into a source set, so the fallback chain has to be complete -- an empty field
// set would leave routing with no constraint at all.
func TestClassify_RequiredFields(t *testing.T) {
	t.Parallel()

	t.Run("defaults come from the request type", func(t *testing.T) {
		t.Parallel()
		c := fixture(t, nil)
		got, err := c.Classify(get("/api/v1/resources/{resourceId}/status"))
		testutil.NoError(t, err, "classify")
		testutil.Equal(t, got.RequiredFields, policy.DefaultFieldsFor(TypeResourceStatus),
			"with neither a caller projection nor a configured set, the declared defaults apply")
	})

	t.Run("configuration overrides the defaults", func(t *testing.T) {
		t.Parallel()
		c := fixture(t, func(cfg *config.Config) {
			rule := cfg.Routing.RequestTypes[TypeResourceStatus]
			rule.RequiredFields = []string{policy.FieldConfiguration}
			cfg.Routing.RequestTypes[TypeResourceStatus] = rule
		})
		got, err := c.Classify(get("/api/v1/resources/{resourceId}/status"))
		testutil.NoError(t, err, "classify")
		testutil.Equal(t, got.RequiredFields, []string{policy.FieldConfiguration},
			"a deployment can change what an endpoint must produce without a release")
	})

	t.Run("the caller overrides configuration", func(t *testing.T) {
		t.Parallel()
		c := fixture(t, func(cfg *config.Config) {
			rule := cfg.Routing.RequestTypes[TypeResourceStatus]
			rule.RequiredFields = []string{policy.FieldConfiguration}
			cfg.Routing.RequestTypes[TypeResourceStatus] = rule
		})
		in := get("/api/v1/resources/{resourceId}/status")
		in.RequestedFields = []string{policy.FieldMetrics}
		got, err := c.Classify(in)
		testutil.NoError(t, err, "classify")
		testutil.Equal(t, got.RequiredFields, []string{policy.FieldMetrics}, "the projection wins")
	})
}

// TestClassify_MissingRoutingRuleIsInternal verifies REQ-CFG-004: a request
// type the routing configuration does not know is a deployment error, not a
// client error. Reporting it as a 400 would blame the caller for a
// misconfigured service and hide the outage from whoever can fix it.
func TestClassify_MissingRoutingRuleIsInternal(t *testing.T) {
	t.Parallel()

	c := fixture(t, func(cfg *config.Config) {
		delete(cfg.Routing.RequestTypes, TypeResourceStatus)
	})

	_, err := c.Classify(get("/api/v1/resources/{resourceId}/status"))
	testutil.Error(t, err, "the request cannot be routed")
	testutil.ErrCode(t, err, errs.CodeInternal,
		"the route exists and the caller did nothing wrong; the service is misconfigured")

	e, ok := errs.As(err)
	testutil.True(t, ok, "structured detail is attached")
	testutil.Equal(t, e.Details["request_type"], any(TypeResourceStatus),
		"the failure names the request type that has no policy, so it can be fixed")

	t.Run("the other routes still work", func(t *testing.T) {
		t.Parallel()
		_, err := c.Classify(get("/api/v1/resources/{resourceId}/details"))
		testutil.NoError(t, err, "one missing rule does not disable the rest of the API")
	})
}

// TestClassify_CarriesTheRequestEnvelope verifies REQ-CLS-003 and REQ-CLS-004:
// everything the router and the handlers need travels in one classification, so
// no downstream component has to re-read configuration or re-parse the request.
func TestClassify_CarriesTheRequestEnvelope(t *testing.T) {
	t.Parallel()

	c := fixture(t, nil)
	in := get("/api/v1/resources/{resourceId}/executions/{executionId}")
	in.ExecutionID = "E-1"
	in.IncludeAudit = true

	got, err := c.Classify(in)
	testutil.NoError(t, err, "classify")
	testutil.Equal(t, got.Type, TypeExecutionStatus, "request type")
	testutil.Equal(t, got.ExecutionID, "E-1", "the execution id is carried through")
	testutil.True(t, got.IncludeAudit, "as is the audit request, which RBAC still has to approve")
	testutil.Equal(t, got.MaxLatency, 2*time.Second,
		"the configured latency budget travels with the classification")
}

// TestRoutes_ReturnsACopy verifies that the route table cannot be edited
// through the accessor. The API layer walks it to register handlers, and a
// caller that mutated it would silently re-point a live route at another
// request type -- and therefore at another routing policy.
func TestRoutes_ReturnsACopy(t *testing.T) {
	t.Parallel()

	c := fixture(t, nil)
	routes := c.Routes()

	routes["GET /api/v1/resources/{resourceId}/status"] = TypeExecutionHistory
	routes["GET /api/v1/injected"] = TypeResourceRead
	delete(routes, "GET /api/v1/resources/{resourceId}/details")

	after := c.Routes()
	testutil.Equal(t, after["GET /api/v1/resources/{resourceId}/status"], TypeResourceStatus,
		"the classifier's own table is unchanged")
	testutil.Equal(t, after["GET /api/v1/resources/{resourceId}/details"], TypeResourceDetails,
		"a deletion through the copy did not reach it either")
	_, injected := after["GET /api/v1/injected"]
	testutil.False(t, injected, "and no route could be added through the copy")

	got, err := c.Classify(get("/api/v1/resources/{resourceId}/status"))
	testutil.NoError(t, err, "classify")
	testutil.Equal(t, got.Type, TypeResourceStatus, "classification is unaffected")
}

// TestParseConsistency verifies REQ-CLS-004: the tokens are a closed set, and
// the ordering of the levels is what makes "tighten only" expressible as a
// comparison.
func TestParseConsistency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in    string
		want  Consistency
		valid bool
	}{
		{"strong", ConsistencyStrong, true},
		{"bounded", ConsistencyBounded, true},
		{"eventual", ConsistencyEventual, true},
		{"", ConsistencyEventual, true},
		{"  Strong  ", ConsistencyStrong, true},
		{"BOUNDED", ConsistencyBounded, true},
		{"immediate", ConsistencyEventual, false},
		{"none", ConsistencyEventual, false},
	}
	for _, tc := range cases {
		t.Run("token_"+tc.in, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseConsistency(tc.in)
			testutil.Equal(t, ok, tc.valid, "validity of %q", tc.in)
			testutil.Equal(t, got, tc.want, "level for %q", tc.in)
		})
	}

	t.Run("levels are ordered", func(t *testing.T) {
		t.Parallel()
		testutil.True(t, ConsistencyEventual < ConsistencyBounded && ConsistencyBounded < ConsistencyStrong,
			"the tighten-only rule is a comparison, so the order has to hold")
		testutil.Equal(t, ConsistencyStrong.String(), "strong", "rendered for logs and metric attributes")
		testutil.Equal(t, ConsistencyBounded.String(), "bounded", "rendered")
		testutil.Equal(t, ConsistencyEventual.String(), "eventual", "rendered")
	})
}
