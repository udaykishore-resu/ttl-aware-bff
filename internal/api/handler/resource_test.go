package handler_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/api"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/application"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/cache"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/router"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/security"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// ---------------------------------------------------------------------------
// Harness
//
// The handler is driven through the real server stack -- middleware, mux,
// classifier, application service -- with the two data sources replaced by
// fakes. Anything less would test the handler's parse function rather than the
// endpoint's behaviour, and it is the endpoint's behaviour the API contract
// promises.
// ---------------------------------------------------------------------------

const (
	handlerSecret = "handler-test-hs256-secret-32-bytes-long"
	handlerIssuer = "https://issuer.handler.invalid"
	handlerAud    = "ttl-aware-bff"
	testTenant    = "tenant-a"
	testResource  = "res-1"
)

type harness struct {
	handler http.Handler
	ops     *testutil.FakeOperational
	execs   *testutil.FakeExecution
	cfg     *config.Provider
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Observability.Environment = "test"
	cfg.Security.JWT.Issuer = handlerIssuer
	cfg.Security.JWT.Audience = handlerAud
	cfg.Security.JWT.HS256SecretEnv = "BFF_TEST_UNUSED"
	// Admission control is exercised in the resilience tests; here it would only
	// make a long table of requests flaky.
	cfg.Security.RateLimit.Enabled = false
	if mutate != nil {
		mutate(&cfg)
	}
	testutil.NoError(t, config.Validate(&cfg), "the harness configuration must be valid")

	provider := config.NewProvider(cfg)
	ops := testutil.NewFakeOperational()
	execs := testutil.NewFakeExecution()

	catalog := policy.NewFieldCatalog(cfg.Precedence)
	prec := policy.NewPrecedence(cfg.Precedence, nil)
	fresh := freshness.NewManager(ops, cfg.Routing.Defaults.ProbeCacheTTL.D(), freshness.NoopHooks())
	rt := router.New(provider, catalog, fresh, router.NoopHooks())

	backend, err := cache.Build(cfg.Cache)
	testutil.NoError(t, err, "building the cache backend")
	t.Cleanup(func() {
		if backend != nil {
			_ = backend.Close()
		}
	})

	obs := observability.NewNoopProvider()
	// Discard logs: the handlers log every refusal, and a table of 400s would
	// otherwise bury the test output.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := application.New(application.Deps{
		Config:      provider,
		Router:      rt,
		Operational: ops,
		Execution:   execs,
		Precedence:  prec,
		Catalog:     catalog,
		Cache:       cache.NewManager(backend, cfg.Cache, cache.NoopHooks()),
		Observer:    obs,
		Logger:      log,
	})

	keys, err := security.NewHMACKeyProvider(handlerSecret)
	testutil.NoError(t, err, "building the key provider")

	srv := api.New(api.Deps{
		Config:      provider,
		Logger:      log,
		Observer:    obs,
		Service:     svc,
		Classifier:  classifier.New(provider, catalog),
		Auth:        security.NewAuthenticator(cfg.Security, keys),
		RateLimiter: resilience.NewRateLimiter(cfg.Security.RateLimit),
		Build:       api.BuildInfo{Version: "test"},
	})

	return &harness{handler: srv.PublicHandler(), ops: ops, execs: execs, cfg: provider}
}

// seedResource makes a resource available from the operational fake.
func (h *harness) seedResource(observedAt time.Time) {
	h.ops.Resources[testResource] = &domain.Resource{
		TenantID:      testTenant,
		ResourceID:    testResource,
		CustomerID:    "cust-42",
		Type:          "database",
		Status:        domain.StatusActive,
		SubState:      "STEADY",
		Configuration: map[string]string{"tier": "gold"},
		ObservedAt:    observedAt,
	}
}

// token mints a bearer token carrying the named roles.
func token(t *testing.T, roles ...string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":       handlerIssuer,
		"aud":       handlerAud,
		"sub":       "user-1",
		"tenant_id": testTenant,
		"roles":     roles,
		"jti":       "token-1",
		"iat":       now.Add(-time.Minute).Unix(),
		"exp":       now.Add(time.Hour).Unix(),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(handlerSecret))
	testutil.NoError(t, err, "signing the test token")
	return raw
}

// get issues an authenticated GET and returns the recorded response.
func (h *harness) get(t *testing.T, path string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{"bff.viewer"}
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token(t, roles...))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// errorDoc is the failure document as a client sees it.
type errorDoc struct {
	Error struct {
		Code          errs.Code `json:"code"`
		Type          string    `json:"type"`
		Title         string    `json:"title"`
		Status        int       `json:"status"`
		Detail        string    `json:"detail"`
		CorrelationID string    `json:"correlationId"`
		Retryable     bool      `json:"retryable"`
		Sources       []struct {
			Source string `json:"source"`
			State  string `json:"state"`
		} `json:"sources"`
	} `json:"error"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorDoc {
	t.Helper()
	var doc errorDoc
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc), "the failure body must be the service's error document")
	return doc
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

// TestResource_RejectsUnknownQueryParameter verifies REQ-API-007: an
// unrecognised query parameter is a 400, not something quietly ignored. A typo
// in a client's URL must surface as an error rather than as silently different
// behaviour.
func TestResource_RejectsUnknownQueryParameter(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{name: "misspelled limit", query: "?limt=10", want: http.StatusBadRequest},
		{name: "unknown parameter", query: "?expand=all", want: http.StatusBadRequest},
		{name: "case matters", query: "?Limit=10", want: http.StatusBadRequest},
		{name: "known parameter with an empty value is still known", query: "?cursor=", want: http.StatusOK},
		{name: "unknown parameter alongside a known one", query: "?limit=5&debug=1", want: http.StatusBadRequest},
		{name: "no parameters", query: "", want: http.StatusOK},
		{name: "every known parameter", query: "?fields=status&consistency=bounded&limit=5&cursor=abc&includeAudit=false", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, "/api/v1/resources/"+testResource+"/status"+tc.query)
			testutil.Equal(t, rec.Code, tc.want, "status for %q\n  body: %s", tc.query, rec.Body.String())
			if tc.want != http.StatusBadRequest {
				return
			}
			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, errs.CodeInvalidRequest, "error code")
			testutil.Equal(t, doc.Error.Detail, "unrecognised query parameter", "the client is told what was wrong")
		})
	}
}

// TestResource_ValidatesPathIdentifiers verifies REQ-SEC-009: path identifiers
// are charset- and length-checked before anything else touches them, so a
// hostile identifier never reaches a data source.
func TestResource_ValidatesPathIdentifiers(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	cases := []struct {
		name string
		id   string
		want int
	}{
		{name: "plain", id: "res-1", want: http.StatusOK},
		{name: "every permitted character class", id: "abcXYZ012-_.:", want: http.StatusNotFound},
		{name: "space", id: "res%201", want: http.StatusBadRequest},
		{name: "slash-escaped traversal", id: "..%2Fetc%2Fpasswd", want: http.StatusBadRequest},
		{name: "sql-ish", id: "res-1'or'1'='1", want: http.StatusBadRequest},
		{name: "angle brackets", id: "%3Cscript%3E", want: http.StatusBadRequest},
		{name: "asterisk", id: "res-*", want: http.StatusBadRequest},
		{name: "percent", id: "res-%25", want: http.StatusBadRequest},
		{name: "newline", id: "res%0A1", want: http.StatusBadRequest},
		{name: "null byte", id: "res%001", want: http.StatusBadRequest},
		{name: "non-ascii", id: "res-%C3%A9", want: http.StatusBadRequest},
		{name: "at the length limit", id: strings.Repeat("a", 128), want: http.StatusNotFound},
		{name: "one past the length limit", id: strings.Repeat("a", 129), want: http.StatusBadRequest},
		{name: "far too long", id: strings.Repeat("a", 4096), want: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, "/api/v1/resources/"+tc.id+"/status")
			testutil.Equal(t, rec.Code, tc.want, "status for id %q\n  body: %s", tc.id, rec.Body.String())
			if tc.want != http.StatusBadRequest {
				return
			}
			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, errs.CodeInvalidRequest, "error code")
			testutil.True(t, strings.HasPrefix(doc.Error.Detail, "resourceId "),
				"the message names the offending parameter, got %q", doc.Error.Detail)
			testutil.False(t, strings.Contains(doc.Error.Detail, tc.id),
				"the rejected value must not be echoed back into the body")
		})
	}
}

// TestResource_ValidatesExecutionIdentifier verifies REQ-SEC-009 for the nested
// path parameter, which is validated on exactly the same terms.
func TestResource_ValidatesExecutionIdentifier(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	rec := h.get(t, "/api/v1/resources/"+testResource+"/executions/exec%20one")
	testutil.Equal(t, rec.Code, http.StatusBadRequest, "an execution id with a space is refused: %s", rec.Body.String())
	doc := decodeError(t, rec)
	testutil.True(t, strings.HasPrefix(doc.Error.Detail, "executionId "),
		"the message names the offending parameter, got %q", doc.Error.Detail)

	rec = h.get(t, "/api/v1/resources/"+testResource+"/executions/"+strings.Repeat("e", 129))
	testutil.Equal(t, rec.Code, http.StatusBadRequest, "an over-long execution id is refused")
}

// TestResource_ValidatesLimit verifies REQ-API-014: the page size is bounded and
// must be a positive integer. limit=0 and limit=abc are both refusals, not
// silent defaults.
func TestResource_ValidatesLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	cases := []struct {
		name  string
		limit string
		want  int
	}{
		{name: "zero", limit: "0", want: http.StatusBadRequest},
		{name: "non-numeric", limit: "abc", want: http.StatusBadRequest},
		{name: "negative", limit: "-1", want: http.StatusBadRequest},
		{name: "float", limit: "1.5", want: http.StatusBadRequest},
		{name: "whitespace", limit: "%20", want: http.StatusBadRequest},
		{name: "hex", limit: "0x10", want: http.StatusBadRequest},
		{name: "overflowing", limit: "99999999999999999999", want: http.StatusBadRequest},
		{name: "one", limit: "1", want: http.StatusOK},
		{name: "twenty five", limit: "25", want: http.StatusOK},
		// An absent limit is not an invalid one; the classifier supplies the
		// configured page size.
		{name: "absent", limit: "", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/resources/" + testResource + "/executions"
			if tc.limit != "" {
				path += "?limit=" + tc.limit
			}
			rec := h.get(t, path)
			testutil.Equal(t, rec.Code, tc.want, "status for limit=%q\n  body: %s", tc.limit, rec.Body.String())
			if tc.want != http.StatusBadRequest {
				return
			}
			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, errs.CodeInvalidRequest, "error code")
			testutil.Equal(t, doc.Error.Detail, "limit must be a positive integer", "message")
		})
	}
}

// TestResource_ValidatesConsistency verifies REQ-CLS-005: the consistency
// override is drawn from a closed set, and an unrecognised value is a 400
// rather than a silent fallback to the endpoint's default.
func TestResource_ValidatesConsistency(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	cases := []struct {
		name  string
		value string
		want  int
	}{
		{name: "nonsense", value: "nonsense", want: http.StatusBadRequest},
		{name: "close but wrong", value: "eventualy", want: http.StatusBadRequest},
		{name: "numeric", value: "1", want: http.StatusBadRequest},
		{name: "bounded", value: "bounded", want: http.StatusOK},
		{name: "eventual", value: "eventual", want: http.StatusOK},
		{name: "case insensitive", value: "BOUNDED", want: http.StatusOK},
		{name: "padded", value: "%20bounded%20", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, "/api/v1/resources/"+testResource+"/status?consistency="+tc.value)
			testutil.Equal(t, rec.Code, tc.want, "status for consistency=%q\n  body: %s", tc.value, rec.Body.String())
			if tc.want != http.StatusBadRequest {
				return
			}
			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, errs.CodeInvalidRequest, "error code")
			testutil.Equal(t, doc.Error.Detail, "consistency must be one of strong, bounded, eventual", "message")
		})
	}
}

// TestResource_ValidatesFields verifies REQ-CLS-004: a projection naming a field
// the BFF does not model is a request error, so a client learns about its typo
// instead of quietly receiving a narrower response.
func TestResource_ValidatesFields(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	rec := h.get(t, "/api/v1/resources/"+testResource+"/status?fields=status,subState")
	testutil.Equal(t, rec.Code, http.StatusOK, "known fields are accepted: %s", rec.Body.String())

	rec = h.get(t, "/api/v1/resources/"+testResource+"/status?fields=status,vibes")
	testutil.Equal(t, rec.Code, http.StatusBadRequest, "an unknown field is refused")
	testutil.Equal(t, decodeError(t, rec).Error.Detail, "unknown field requested", "message")

	rec = h.get(t, "/api/v1/resources/"+testResource+"/status?fields="+strings.TrimSuffix(strings.Repeat("status,", 33), ","))
	testutil.Equal(t, rec.Code, http.StatusBadRequest, "an absurd projection is refused")
	testutil.Equal(t, decodeError(t, rec).Error.Detail, "too many fields requested", "message")
}

// TestResource_ValidatesCursor verifies REQ-API-014: the pagination cursor is
// length-bounded.
func TestResource_ValidatesCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	rec := h.get(t, "/api/v1/resources/"+testResource+"/executions?cursor="+strings.Repeat("c", 512))
	testutil.Equal(t, rec.Code, http.StatusOK, "a cursor at the limit is accepted: %s", rec.Body.String())

	rec = h.get(t, "/api/v1/resources/"+testResource+"/executions?cursor="+strings.Repeat("c", 513))
	testutil.Equal(t, rec.Code, http.StatusBadRequest, "an over-long cursor is refused")
	testutil.Equal(t, decodeError(t, rec).Error.Detail, "cursor is too long", "message")
}

// ---------------------------------------------------------------------------
// Authorisation
// ---------------------------------------------------------------------------

// TestResource_IncludeAuditRequiresPermission verifies REQ-SEC-005: asking for
// audit data the caller may not see is a 403, never a silently narrowed 200. A
// UI that believes it received audit records when it did not is worse than an
// explicit refusal.
func TestResource_IncludeAuditRequiresPermission(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())
	path := "/api/v1/resources/" + testResource + "/executions"

	t.Run("without the audit permission", func(t *testing.T) {
		rec := h.get(t, path+"?includeAudit=true", "bff.viewer")
		testutil.Equal(t, rec.Code, http.StatusForbidden,
			"the request must be refused outright, not narrowed\n  body: %s", rec.Body.String())

		doc := decodeError(t, rec)
		testutil.Equal(t, doc.Error.Code, errs.CodeForbidden, "error code")
		testutil.Equal(t, doc.Error.Status, http.StatusForbidden, "the document repeats the status")
		testutil.Equal(t, doc.Error.Detail, "the caller is not permitted to read audit information", "message")
		testutil.False(t, strings.Contains(rec.Body.String(), "\"data\""),
			"a refusal must not carry a data envelope")
	})

	t.Run("with the audit permission", func(t *testing.T) {
		rec := h.get(t, path+"?includeAudit=true", "bff.operator")
		testutil.Equal(t, rec.Code, http.StatusOK, "an operator may read audit data: %s", rec.Body.String())
	})

	t.Run("includeAudit=false needs no permission", func(t *testing.T) {
		rec := h.get(t, path+"?includeAudit=false", "bff.viewer")
		testutil.Equal(t, rec.Code, http.StatusOK, "not asking for audit data is always allowed: %s", rec.Body.String())
	})

	t.Run("a malformed includeAudit is a 400, checked before the permission", func(t *testing.T) {
		rec := h.get(t, path+"?includeAudit=perhaps", "bff.viewer")
		testutil.Equal(t, rec.Code, http.StatusBadRequest, "an unparseable boolean is a request error")
		testutil.Equal(t, decodeError(t, rec).Error.Detail, "includeAudit must be true or false", "message")
	})

	t.Run("the truthy spellings ParseBool accepts", func(t *testing.T) {
		for _, v := range []string{"1", "t", "T", "TRUE", "True"} {
			rec := h.get(t, path+"?includeAudit="+v, "bff.viewer")
			testutil.Equal(t, rec.Code, http.StatusForbidden,
				"%q means true and must be refused for a viewer", v)
		}
	})
}

// TestResource_EndpointPermissions verifies REQ-SEC-005: each endpoint declares
// the permission it needs, and default_deny turns a missing grant into a 403.
func TestResource_EndpointPermissions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	base := "/api/v1/resources/" + testResource
	cases := []struct {
		path         string
		allowedRole  string
		refusedRoles []string
	}{
		{path: base, allowedRole: "bff.viewer", refusedRoles: []string{"bff.nobody"}},
		{path: base + "/status", allowedRole: "bff.viewer", refusedRoles: []string{"bff.nobody"}},
		{path: base + "/configuration", allowedRole: "bff.viewer", refusedRoles: []string{"bff.nobody"}},
		{path: base + "/details", allowedRole: "bff.viewer", refusedRoles: []string{"bff.nobody"}},
		{path: base + "/executions", allowedRole: "bff.viewer", refusedRoles: []string{"bff.nobody"}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			for _, role := range tc.refusedRoles {
				rec := h.get(t, tc.path, role)
				testutil.Equal(t, rec.Code, http.StatusForbidden,
					"%s without a grant must be refused\n  body: %s", tc.path, rec.Body.String())
				testutil.Equal(t, decodeError(t, rec).Error.Code, errs.CodeForbidden, "error code")
			}
			rec := h.get(t, tc.path, tc.allowedRole)
			testutil.NotEqual(t, rec.Code, http.StatusForbidden, "%s with a grant must not be refused", tc.path)
		})
	}

	t.Run("with default_deny off an ungranted permission is allowed", func(t *testing.T) {
		t.Parallel()
		relaxed := newHarness(t, func(c *config.Config) { c.Security.RBAC.DefaultDeny = false })
		relaxed.seedResource(time.Now())
		rec := relaxed.get(t, base+"/status", "bff.nobody")
		testutil.Equal(t, rec.Code, http.StatusOK,
			"default_deny off makes the permission check advisory: %s", rec.Body.String())
	})
}

// TestResource_RequiresAuthentication verifies REQ-SEC-001: the data plane is
// authenticated, and an anonymous request never reaches a handler.
func TestResource_RequiresAuthentication(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/resources/"+testResource+"/status", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	testutil.Equal(t, rec.Code, http.StatusUnauthorized, "an anonymous request is refused")
	testutil.Equal(t, decodeError(t, rec).Error.Code, errs.CodeUnauthenticated, "error code")
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 0, "no source was consulted")
}

// TestResource_RefusesTenantHeaderMismatch verifies REQ-MT-001: a header may
// assert a tenant and is checked against the token, but a mismatch is a refusal
// rather than a resolution in either direction.
func TestResource_RefusesTenantHeaderMismatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/resources/"+testResource+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+token(t, "bff.viewer"))
	req.Header.Set("X-Tenant-ID", "tenant-b")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	testutil.Equal(t, rec.Code, http.StatusForbidden, "a cross-tenant assertion is refused")
	testutil.Equal(t, decodeError(t, rec).Error.Code, errs.CodeTenantMismatch, "error code")
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 0, "no source was consulted")
}

// ---------------------------------------------------------------------------
// Success envelope
// ---------------------------------------------------------------------------

// TestResource_SuccessEnvelope verifies REQ-API-004 and REQ-API-005: every 2xx
// body has the same shape -- data plus meta -- and meta carries the documented
// fields, whichever source answered.
func TestResource_SuccessEnvelope(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())

	rec := h.get(t, "/api/v1/resources/"+testResource+"/status")
	testutil.Equal(t, rec.Code, http.StatusOK, "a healthy read succeeds: %s", rec.Body.String())
	testutil.Equal(t, rec.Header().Get("Content-Type"), "application/json; charset=utf-8", "content type")
	testutil.Equal(t, rec.Header().Get("X-Content-Type-Options"), "nosniff", "sniffing is disabled")
	testutil.NotEqual(t, rec.Header().Get("X-Correlation-ID"), "", "the correlation id is echoed")
	testutil.NotEqual(t, rec.Header().Get("X-BFF-Freshness"), "", "the freshness state is advertised in a header")
	testutil.Equal(t, rec.Header().Get("X-BFF-Source"), "OPERATIONAL", "the routing decision is advertised in a header")

	var body map[string]json.RawMessage
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "the body is JSON")
	_, hasData := body["data"]
	_, hasMeta := body["meta"]
	testutil.True(t, hasData, "the envelope carries data")
	testutil.True(t, hasMeta, "the envelope carries meta")
	testutil.Equal(t, len(body), 2, "the envelope has exactly the two documented members")

	var data struct {
		TenantID   string                `json:"tenantId"`
		ResourceID string                `json:"resourceId"`
		Status     domain.ResourceStatus `json:"status"`
		SubState   string                `json:"subState"`
	}
	testutil.NoError(t, json.Unmarshal(body["data"], &data), "data decodes into the canonical projection")
	testutil.Equal(t, data.TenantID, testTenant, "tenantId")
	testutil.Equal(t, data.ResourceID, testResource, "resourceId")
	testutil.Equal(t, data.Status, domain.StatusActive, "status")
	testutil.Equal(t, data.SubState, "STEADY", "subState")

	var meta struct {
		CorrelationID   string   `json:"correlationId"`
		RoutingDecision string   `json:"routingDecision"`
		RoutingRule     string   `json:"routingRule"`
		Sources         []string `json:"sources"`
		Freshness       struct {
			State      string  `json:"state"`
			AgeSeconds float64 `json:"ageSeconds"`
			TTLSeconds float64 `json:"ttlSeconds"`
		} `json:"freshness"`
		Degraded bool `json:"degraded"`
		Partial  bool `json:"partial"`
		Cache    struct {
			Hit   bool   `json:"hit"`
			Layer string `json:"layer"`
		} `json:"cache"`
	}
	testutil.NoError(t, json.Unmarshal(body["meta"], &meta), "meta decodes")

	testutil.Equal(t, meta.CorrelationID, rec.Header().Get("X-Correlation-ID"),
		"the body and the header agree on the correlation id")
	testutil.Equal(t, meta.RoutingDecision, "OPERATIONAL", "routingDecision names the source that answered")
	testutil.NotEqual(t, meta.RoutingRule, "", "routingRule names the rule that decided")
	testutil.Equal(t, meta.Sources, []string{"OPERATIONAL"}, "sources")
	testutil.NotEqual(t, meta.Freshness.State, "", "the freshness state is always present")
	testutil.True(t, meta.Freshness.TTLSeconds > 0, "the applied TTL is reported in seconds")
	testutil.False(t, meta.Partial, "a complete answer is not partial")
	testutil.False(t, meta.Cache.Hit, "the first read is a miss")

	// The freshness and cache sections are reported separately on purpose: a
	// cache TTL and a source freshness TTL are different concepts.
	var metaKeys map[string]json.RawMessage
	testutil.NoError(t, json.Unmarshal(body["meta"], &metaKeys), "meta decodes as an object")
	for _, key := range []string{"correlationId", "routingDecision", "sources", "freshness", "degraded", "partial", "cache"} {
		_, ok := metaKeys[key]
		testutil.True(t, ok, "meta must carry %q", key)
	}
}

// TestResource_CacheHitIsReported verifies REQ-CACHE-001 and REQ-API-005: a
// second read of the same resource is served from the cache and says so, and the
// cache is not consulted in place of the source's freshness verdict.
func TestResource_CacheHitIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())
	path := "/api/v1/resources/" + testResource + "/status"

	first := h.get(t, path)
	testutil.Equal(t, first.Code, http.StatusOK, "first read: %s", first.Body.String())
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 1, "the source was read once")

	second := h.get(t, path)
	testutil.Equal(t, second.Code, http.StatusOK, "second read: %s", second.Body.String())
	testutil.Equal(t, h.ops.CallCount("GetResourceState"), 1, "the second read did not touch the source")

	var body struct {
		Meta struct {
			Sources []string `json:"sources"`
			Cache   struct {
				Hit   bool   `json:"hit"`
				Layer string `json:"layer"`
			} `json:"cache"`
		} `json:"meta"`
	}
	testutil.NoError(t, json.Unmarshal(second.Body.Bytes(), &body), "decode")
	testutil.True(t, body.Meta.Cache.Hit, "the cache hit is reported")
	testutil.Equal(t, body.Meta.Cache.Layer, "L1", "the tier that answered is named")
	testutil.True(t, contains(body.Meta.Sources, "CACHE"), "the cache is recorded as a contributing source")
}

// ---------------------------------------------------------------------------
// Error documents
// ---------------------------------------------------------------------------

// TestResource_ErrorDocumentShape verifies REQ-API-009: every failure uses the
// same RFC 7807-shaped document, closed over the taxonomy's codes.
func TestResource_ErrorDocumentShape(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	cases := []struct {
		name       string
		path       string
		roles      []string
		wantStatus int
		wantCode   errs.Code
		wantTitle  string
		wantSlug   string
		retryable  bool
	}{
		{
			name: "invalid request", path: "/api/v1/resources/" + testResource + "/status?bogus=1",
			wantStatus: http.StatusBadRequest, wantCode: errs.CodeInvalidRequest,
			wantTitle: "Invalid request", wantSlug: "invalid-request",
		},
		{
			name: "forbidden", path: "/api/v1/resources/" + testResource + "/status",
			roles: []string{"bff.nobody"}, wantStatus: http.StatusForbidden, wantCode: errs.CodeForbidden,
			wantTitle: "Access denied", wantSlug: "forbidden",
		},
		{
			name: "not found", path: "/api/v1/resources/absent-resource/status",
			wantStatus: http.StatusNotFound, wantCode: errs.CodeNotFound,
			wantTitle: "Not found", wantSlug: "not-found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, tc.path, tc.roles...)
			testutil.Equal(t, rec.Code, tc.wantStatus, "status\n  body: %s", rec.Body.String())
			testutil.Equal(t, rec.Header().Get("Content-Type"), "application/json; charset=utf-8", "content type")

			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, tc.wantCode, "code")
			testutil.Equal(t, doc.Error.Status, tc.wantStatus, "the document repeats the transport status")
			testutil.Equal(t, doc.Error.Title, tc.wantTitle, "title")
			testutil.Equal(t, doc.Error.Type, "https://errors.bff.internal/"+tc.wantSlug, "documentation type URI")
			testutil.Equal(t, doc.Error.Retryable, tc.retryable, "retryable")
			testutil.Equal(t, doc.Error.CorrelationID, rec.Header().Get("X-Correlation-ID"),
				"the correlation id links the body to the log")
			testutil.NotEqual(t, doc.Error.Detail, "", "a human-readable detail is always present")

			// The failure document is the whole body: no data envelope leaks in.
			var raw map[string]json.RawMessage
			testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "the body is JSON")
			testutil.Equal(t, len(raw), 1, "the failure document has exactly one member")
			_, hasError := raw["error"]
			testutil.True(t, hasError, "and that member is `error`")
		})
	}
}

// TestResource_InternalCauseNeverLeaks verifies REQ-SEC-013: only the taxonomy's
// own message reaches the client. A wrapped cause may name an internal host or a
// source schema, so it is logged and never sent.
func TestResource_InternalCauseNeverLeaks(t *testing.T) {
	t.Parallel()

	const secret = "ods-primary.internal.svc.cluster.local:9101 password=hunter2"

	h := newHarness(t, nil)
	h.seedResource(time.Now())
	h.ops.SetError("GetResourceState", errs.Wrap(errs.CodeInternal,
		"the request could not be completed", errors.New(secret)))

	rec := h.get(t, "/api/v1/resources/"+testResource+"/status")
	testutil.Equal(t, rec.Code, http.StatusInternalServerError, "an internal failure is a 500: %s", rec.Body.String())

	body := rec.Body.String()
	testutil.False(t, strings.Contains(body, secret), "the wrapped cause must never reach the client")
	testutil.False(t, strings.Contains(body, "internal.svc"), "no internal hostname reaches the client")
	testutil.False(t, strings.Contains(body, "hunter2"), "no credential reaches the client")

	doc := decodeError(t, rec)
	testutil.Equal(t, doc.Error.Code, errs.CodeInternal, "code")
	testutil.Equal(t, doc.Error.Title, "Internal error", "title")
	testutil.Equal(t, doc.Error.Detail, "the request could not be completed",
		"only the taxonomy's own message is sent")
	testutil.NotEqual(t, doc.Error.CorrelationID, "", "the correlation id is how an operator finds the real cause")
}

// TestResource_UpstreamFailureReportsSourceStates verifies REQ-API-009: a
// failure caused by a source names each source's condition, which is safe to
// expose because it names the source's role and never its address.
func TestResource_UpstreamFailureReportsSourceStates(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())
	h.ops.SetHealth(false, "CIRCUIT_OPEN")
	h.execs.SetHealth(false, "UNREACHABLE")

	rec := h.get(t, "/api/v1/resources/"+testResource+"/status")
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable,
		"with both sources down and nothing cached the request fails: %s", rec.Body.String())

	doc := decodeError(t, rec)
	testutil.Equal(t, doc.Error.Code, errs.CodeNoSourceAvailable, "code")
	testutil.Equal(t, len(doc.Error.Sources), 2, "both sources are described")

	states := map[string]string{}
	for _, s := range doc.Error.Sources {
		states[s.Source] = s.State
	}
	testutil.Equal(t, states["OPERATIONAL"], "CIRCUIT_OPEN", "the operational source's condition")
	testutil.Equal(t, states["EXECUTION"], "UNREACHABLE", "the execution source's condition")
	testutil.False(t, strings.Contains(rec.Body.String(), "localhost"), "no source address is exposed")
	testutil.False(t, strings.Contains(rec.Body.String(), "9101"), "no source port is exposed")
}

// ---------------------------------------------------------------------------
// Routing surface
// ---------------------------------------------------------------------------

// TestAPI_NotFoundCatchAll verifies REQ-API-009: an unmatched route returns the
// service's own error document rather than Go's plain-text default, so a client
// parses exactly one error shape.
func TestAPI_NotFoundCatchAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	paths := []string{
		"/",
		"/api",
		"/api/v1",
		"/api/v1/resources",
		"/api/v2/resources/res-1",
		"/api/v1/resources/res-1/status/extra",
		"/healthz",
		"/metrics",
		"/etc/passwd",
		"/api/v1/resources/res-1/status/",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+token(t, "bff.viewer"))
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)

			testutil.Equal(t, rec.Code, http.StatusNotFound, "unmatched route %q\n  body: %s", path, rec.Body.String())
			testutil.Equal(t, rec.Header().Get("Content-Type"), "application/json; charset=utf-8",
				"the catch-all must not fall back to Go's text/plain 404")
			testutil.False(t, strings.HasPrefix(rec.Body.String(), "404 page not found"),
				"Go's default 404 body must never be returned")

			doc := decodeError(t, rec)
			testutil.Equal(t, doc.Error.Code, errs.CodeNotFound, "code")
			testutil.Equal(t, doc.Error.Status, http.StatusNotFound, "status")
			testutil.Equal(t, doc.Error.Title, "Not found", "title")
			testutil.Equal(t, doc.Error.Detail, "no such endpoint", "detail")
			testutil.Equal(t, doc.Error.Type, "https://errors.bff.internal/not-found", "type URI")
			testutil.NotEqual(t, doc.Error.CorrelationID, "", "even a 404 is correlated")
		})
	}

	t.Run("a traversal is normalised, then falls through to the catch-all", func(t *testing.T) {
		// net/http's mux cleans the path and redirects; the cleaned target then
		// reaches the catch-all and gets the service's error document.
		req := httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil)
		req.Header.Set("Authorization", "Bearer "+token(t, "bff.viewer"))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		testutil.Equal(t, rec.Code, http.StatusMovedPermanently, "the mux normalises the path")
		testutil.Equal(t, rec.Header().Get("Location"), "/etc/passwd", "and points at the cleaned target")

		follow := h.get(t, "/etc/passwd")
		testutil.Equal(t, follow.Code, http.StatusNotFound, "which is itself an unmatched route")
		testutil.Equal(t, decodeError(t, follow).Error.Code, errs.CodeNotFound, "in the service's own error document")
	})

	t.Run("a wrong method on a real path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/resources/"+testResource+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+token(t, "bff.viewer"))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)

		testutil.Equal(t, rec.Header().Get("Content-Type"), "application/json; charset=utf-8",
			"a method refusal also uses the service's error document")
		testutil.True(t, rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed,
			"a non-GET is refused, got %d", rec.Code)
	})
}

// TestAPI_EveryRegisteredRouteIsClassifiable verifies REQ-CLS-001: the routes
// the server registers and the routes the classifier knows are the same set. A
// route registered here but unknown to the classifier would 400 at runtime for
// every caller.
func TestAPI_EveryRegisteredRouteIsClassifiable(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.seedResource(time.Now())
	h.execs.ByID["exec-1"] = &domain.Execution{
		ExecutionID: "exec-1", TenantID: testTenant, ResourceID: testResource,
		Status: domain.ExecCompleted, Operation: "scale-out",
	}
	h.execs.Latest[testResource] = h.execs.ByID["exec-1"]

	base := "/api/v1/resources/" + testResource
	for _, path := range []string{
		base,
		base + "/status",
		base + "/configuration",
		base + "/details",
		base + "/executions",
		base + "/executions/exec-1",
	} {
		t.Run(path, func(t *testing.T) {
			rec := h.get(t, path, "bff.operator")
			testutil.NotEqual(t, rec.Code, http.StatusBadRequest,
				"%s must be classifiable\n  body: %s", path, rec.Body.String())
			testutil.NotEqual(t, rec.Code, http.StatusInternalServerError,
				"%s must have a routing rule\n  body: %s", path, rec.Body.String())
			testutil.NotEqual(t, rec.Code, http.StatusNotFound,
				"%s must be a registered route\n  body: %s", path, rec.Body.String())
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
