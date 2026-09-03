//go:build integration

// Package integration drives the real, running stack over HTTP: the BFF
// binary, the reference gRPC operational source and the reference REST
// execution source, with the cache backend the deployment is configured with.
//
// These tests are deliberately black-box. They assert only what a UI can
// observe -- status codes, the response envelope, and the routing metadata the
// BFF publishes -- so they keep passing across internal refactors and fail when
// behaviour a client depends on changes.
//
// Run with:
//
//	make compose-up          # or: the three binaries on their default ports
//	go test -tags=integration ./test/integration/...
//
// Every test resets the chaos state it touched, and each uses its own resource
// where the state it sets would otherwise leak between tests.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// ---------------------------------------------------------------------------
// Wire types: a deliberately independent view of the API, so a change to the
// service's own structs cannot quietly change what these tests assert.
// ---------------------------------------------------------------------------

type envelope struct {
	Data json.RawMessage `json:"data"`
	Meta struct {
		CorrelationID   string   `json:"correlationId"`
		RoutingDecision string   `json:"routingDecision"`
		RoutingRule     string   `json:"routingRule"`
		Sources         []string `json:"sources"`
		Freshness       struct {
			State      string  `json:"state"`
			AgeSeconds float64 `json:"ageSeconds"`
			TTLSeconds float64 `json:"ttlSeconds"`
		} `json:"freshness"`
		Degraded   bool               `json:"degraded"`
		Partial    bool               `json:"partial"`
		Cache      struct{ Hit bool } `json:"cache"`
		Provenance map[string]string  `json:"provenance"`
		Warnings   []struct {
			Code   string `json:"code"`
			Source string `json:"source"`
		} `json:"warnings"`
	} `json:"meta"`
}

type errorDocument struct {
	Error struct {
		Code      string `json:"code"`
		Status    int    `json:"status"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func (e envelope) hasWarning(code string) bool {
	for _, w := range e.Meta.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type stack struct {
	bff      string
	opsAdmin string
	edsAdmin string
	client   *http.Client
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{
		bff:      env("BFF_URL", "http://localhost:8080"),
		opsAdmin: env("OPS_ADMIN_URL", "http://localhost:9111"),
		edsAdmin: env("EDS_ADMIN_URL", "http://localhost:9112"),
		client:   &http.Client{Timeout: 15 * time.Second},
	}
	adminURL := env("BFF_ADMIN_URL", "http://localhost:9090")
	resp, err := s.client.Get(adminURL + "/readyz") //nolint:noctx // liveness probe for the suite
	if err != nil {
		t.Skipf("BFF not reachable at %s: %v (run `make compose-up`)", adminURL, err)
	}
	_ = resp.Body.Close()

	// Every test starts from a known-clean chaos state and leaves one behind.
	s.resetChaos(t)
	t.Cleanup(func() { s.resetChaos(t) })
	return s
}

func (s *stack) get(t *testing.T, tenant, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.bff+path, nil) //nolint:noctx // the client carries a timeout
	testutil.NoError(t, err, "build request")
	req.Header.Set("X-Tenant-ID", tenant)
	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "GET %s", path)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	testutil.NoError(t, err, "read body")
	return resp, body
}

func (s *stack) getEnvelope(t *testing.T, tenant, path string) (*http.Response, envelope) {
	t.Helper()
	resp, body := s.get(t, tenant, path)
	var env envelope
	testutil.NoError(t, json.Unmarshal(body, &env), "decode envelope from %s: %s", path, string(body))
	return resp, env
}

func (s *stack) getError(t *testing.T, tenant, path string) (*http.Response, errorDocument) {
	t.Helper()
	resp, body := s.get(t, tenant, path)
	var doc errorDocument
	testutil.NoError(t, json.Unmarshal(body, &doc), "decode error document: %s", string(body))
	return resp, doc
}

func (s *stack) chaos(t *testing.T, admin string, patch map[string]any) {
	t.Helper()
	raw, err := json.Marshal(patch)
	testutil.NoError(t, err, "encode chaos patch")
	req, err := http.NewRequest(http.MethodPut, admin+"/chaos", bytes.NewReader(raw)) //nolint:noctx
	testutil.NoError(t, err, "build chaos request")
	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "PUT %s/chaos", admin)
	_ = resp.Body.Close()
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "chaos update")
}

func (s *stack) resetChaos(t *testing.T) {
	t.Helper()
	for _, admin := range []string{s.opsAdmin, s.edsAdmin} {
		req, err := http.NewRequest(http.MethodDelete, admin+"/chaos", nil) //nolint:noctx
		if err != nil {
			continue
		}
		if resp, err := s.client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
}

// age moves one resource's apparent freshness without touching any other.
func (s *stack) age(t *testing.T, resourceID string, seconds float64) {
	t.Helper()
	url := fmt.Sprintf("%s/resources/%s/age?seconds=%g", s.opsAdmin, resourceID, seconds)
	req, err := http.NewRequest(http.MethodPost, url, nil) //nolint:noctx
	testutil.NoError(t, err, "build age request")
	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "age %s", resourceID)
	_ = resp.Body.Close()
}

func (s *stack) touch(t *testing.T, resourceID string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.opsAdmin+"/resources/"+resourceID+"/touch", nil) //nolint:noctx
	testutil.NoError(t, err, "build touch request")
	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "touch %s", resourceID)
	_ = resp.Body.Close()
}

// waitForBreaker gives the circuit breaker time to observe an injected outage.
// It polls rather than sleeping a fixed interval so the suite is not tuned to
// one machine's speed.
func (s *stack) waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, desc)
}

// tenantOf mirrors the reference sources' seeding: they distribute resources
// across tenants by index, so a test that picks a resource must use its tenant.
func tenantOf(index int) string {
	switch index % 3 {
	case 0:
		return "acme"
	case 1:
		return "local"
	default:
		return "globex"
	}
}

func resourceID(index int) string { return fmt.Sprintf("R%03d", index) }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_FreshOperationalIsServedFromTheFastSource verifies
// REQ-RT-008 end to end through the real HTTP surface.
func TestIntegration_FreshOperationalIsServedFromTheFastSource(t *testing.T) {
	s := newStack(t)

	const idx = 7 // 7%7 == 0, so this resource is seeded with zero age
	s.touch(t, resourceID(idx))

	resp, env := s.getEnvelope(t, tenantOf(idx), "/api/v1/resources/"+resourceID(idx)+"/status")
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "status code")
	testutil.Equal(t, env.Meta.RoutingDecision, "OPERATIONAL", "routing decision")
	testutil.Equal(t, env.Meta.Freshness.State, "FRESH", "freshness")
	testutil.False(t, env.Meta.Degraded, "not degraded")
	testutil.Equal(t, resp.Header.Get("X-BFF-Source"), "OPERATIONAL", "the header agrees with the body")
	testutil.NotEqual(t, resp.Header.Get("X-Correlation-ID"), "", "a correlation id is always returned")
}

// TestIntegration_TTLMissRoutesToTheExecutionSource verifies REQ-RT-009 end to
// end: ageing one resource past its TTL flips the routing decision, and
// refreshing it flips it back. Nothing but the data's age changes.
func TestIntegration_TTLMissRoutesToTheExecutionSource(t *testing.T) {
	s := newStack(t)

	const idx = 14 // 14%7 == 0: zero seeded age, and 14%3==2 -> globex
	id, tenant := resourceID(idx), tenantOf(idx)
	s.touch(t, id)

	// globex configures resource_status with a 15s TTL and a 5s cache TTL.
	_, before := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
	testutil.Equal(t, before.Meta.RoutingDecision, "OPERATIONAL", "a fresh record is served operationally")

	s.age(t, id, 600)
	// Wait out the response cache so the next request re-routes rather than
	// replaying the cached answer.
	s.waitFor(t, 15*time.Second, "the cached answer to expire", func() bool {
		_, e := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
		return e.Meta.RoutingDecision == "EXECUTION"
	})

	_, after := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
	testutil.Equal(t, after.Meta.RoutingDecision, "EXECUTION", "past the TTL the execution source answers")
	testutil.Equal(t, after.Meta.RoutingRule, "ttl.operational.stale", "and the rule that decided says so")

	s.touch(t, id)
	s.waitFor(t, 15*time.Second, "routing to return to the operational source", func() bool {
		_, e := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
		return e.Meta.RoutingDecision == "OPERATIONAL"
	})
}

// TestIntegration_DetailsFansOutToBothSources verifies REQ-AGG-001: the
// /details endpoint reads both sources and reports both in its metadata.
func TestIntegration_DetailsFansOutToBothSources(t *testing.T) {
	s := newStack(t)

	const idx = 3
	id, tenant := resourceID(idx), tenantOf(idx)

	resp, env := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/details")
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "status code")
	testutil.True(t, len(env.Meta.Sources) >= 1, "at least one source contributed")
	testutil.True(t, len(env.Meta.Provenance) > 0, "per-field provenance is reported")

	var body map[string]any
	testutil.NoError(t, json.Unmarshal(env.Data, &body), "decode payload")
	testutil.NotEqual(t, body["status"], nil, "the operational half is present")
}

// TestIntegration_PartialResponseWhenTheOptionalSourceIsDown verifies
// REQ-EDGE-004: /details answers 206 with a warning rather than failing.
func TestIntegration_PartialResponseWhenTheOptionalSourceIsDown(t *testing.T) {
	s := newStack(t)

	const idx = 9
	id, tenant := resourceID(idx), tenantOf(idx)

	s.chaos(t, s.edsAdmin, map[string]any{"unavailable": true})

	var env envelope
	var resp *http.Response
	s.waitFor(t, 20*time.Second, "a partial response", func() bool {
		resp, env = s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/details")
		return resp.StatusCode == http.StatusPartialContent
	})

	testutil.Equal(t, resp.StatusCode, http.StatusPartialContent,
		"a missing optional source is a partial answer, not an error")
	testutil.True(t, env.Meta.Partial, "and is marked partial")
	testutil.True(t, env.hasWarning("SOURCE_UNAVAILABLE") || env.hasWarning("SOURCE_TIMEOUT"),
		"with a warning naming the reason")

	var body map[string]any
	testutil.NoError(t, json.Unmarshal(env.Data, &body), "decode payload")
	testutil.NotEqual(t, body["status"], nil, "the operational half is still served")
}

// TestIntegration_FallbackWhenTheOperationalSourceFails verifies REQ-RES-006:
// the request is answered from the fallback source, marked degraded.
func TestIntegration_FallbackWhenTheOperationalSourceFails(t *testing.T) {
	s := newStack(t)

	const idx = 6
	id, tenant := resourceID(idx), tenantOf(idx)

	s.chaos(t, s.opsAdmin, map[string]any{"unavailable": true})

	var env envelope
	s.waitFor(t, 20*time.Second, "a fallback answer", func() bool {
		resp, e := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
		env = e
		return resp.StatusCode == http.StatusOK && e.Meta.RoutingDecision == "EXECUTION"
	})

	testutil.Equal(t, env.Meta.RoutingDecision, "EXECUTION", "the fallback source answered")
	testutil.True(t, env.Meta.Degraded, "and the answer is marked degraded")
}

// TestIntegration_StaleCacheServedWhenEverySourceIsDown verifies REQ-RES-007
// and REQ-EDGE-005: the last rung of the degradation ladder.
func TestIntegration_StaleCacheServedWhenEverySourceIsDown(t *testing.T) {
	s := newStack(t)

	// acme permits stale answers for up to 60s past the TTL.
	const idx = 12
	id, tenant := resourceID(idx), tenantOf(idx)
	s.touch(t, id)

	resp, _ := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "warm the cache")

	s.chaos(t, s.opsAdmin, map[string]any{"unavailable": true})
	s.chaos(t, s.edsAdmin, map[string]any{"unavailable": true})

	var env envelope
	s.waitFor(t, 25*time.Second, "a degraded answer from cache", func() bool {
		r, e := s.getEnvelope(t, tenant, "/api/v1/resources/"+id+"/status")
		env = e
		return r.StatusCode == http.StatusOK && e.Meta.RoutingRule == "degrade.stale_cache"
	})

	testutil.True(t, env.Meta.Degraded, "the answer is degraded")
	testutil.True(t, env.hasWarning("STALE_DATA"), "and says so with a warning the UI can render")
	testutil.True(t, env.Meta.Cache.Hit, "it came from the cache")
}

// TestIntegration_TenantPolicyChangesTheOutcome verifies REQ-MT-005: two
// tenants with different staleness policies get different answers to the same
// outage, purely from configuration.
func TestIntegration_TenantPolicyChangesTheOutcome(t *testing.T) {
	s := newStack(t)

	// globex is configured allow_stale: false; acme allows it.
	const globexIdx, acmeIdx = 5, 6 // 5%3==2 -> globex, 6%3==0 -> acme
	globexID, acmeID := resourceID(globexIdx), resourceID(acmeIdx)

	// Warm both caches.
	s.getEnvelope(t, "globex", "/api/v1/resources/"+globexID+"/status")
	s.getEnvelope(t, "acme", "/api/v1/resources/"+acmeID+"/status")

	s.chaos(t, s.opsAdmin, map[string]any{"unavailable": true})
	s.chaos(t, s.edsAdmin, map[string]any{"unavailable": true})

	var globexStatus int
	s.waitFor(t, 25*time.Second, "globex to refuse rather than serve stale data", func() bool {
		r, _ := s.get(t, "globex", "/api/v1/resources/"+globexID+"/status")
		globexStatus = r.StatusCode
		return globexStatus == http.StatusServiceUnavailable
	})
	testutil.Equal(t, globexStatus, http.StatusServiceUnavailable,
		"a tenant that forbids stale data gets an error")

	r, env := s.getEnvelope(t, "acme", "/api/v1/resources/"+acmeID+"/status")
	testutil.Equal(t, r.StatusCode, http.StatusOK,
		"while a tenant that permits it gets a degraded answer to the very same outage")
	testutil.True(t, env.Meta.Degraded, "clearly marked")
}

// TestIntegration_ErrorDocumentShape verifies REQ-ERR-001: every failure uses
// one document shape, including the routes Go would otherwise answer in plain
// text.
func TestIntegration_ErrorDocumentShape(t *testing.T) {
	s := newStack(t)

	cases := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"unknown endpoint", "/api/v1/nope", http.StatusNotFound, "NOT_FOUND"},
		{"unknown resource", "/api/v1/resources/no-such-resource/status", http.StatusNotFound, "NOT_FOUND"},
		{"illegal resource id", "/api/v1/resources/bad%20id/status", http.StatusBadRequest, "INVALID_REQUEST"},
		{"unknown query parameter", "/api/v1/resources/R001/status?nope=1", http.StatusBadRequest, "INVALID_REQUEST"},
		{"unknown field", "/api/v1/resources/R001/status?fields=nonsense", http.StatusBadRequest, "INVALID_REQUEST"},
		{"bad limit", "/api/v1/resources/R001/executions?limit=abc", http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, doc := s.getError(t, "local", tc.path)
			testutil.Equal(t, resp.StatusCode, tc.status, "status code")
			testutil.Equal(t, doc.Error.Code, tc.code, "error code")
			testutil.Equal(t, doc.Error.Status, tc.status, "the document repeats the status")
			testutil.Equal(t, resp.Header.Get("Content-Type"), "application/json; charset=utf-8",
				"errors are JSON like everything else")
		})
	}
}

// TestIntegration_CorrelationIDIsEchoed verifies REQ-API-006.
func TestIntegration_CorrelationIDIsEchoed(t *testing.T) {
	s := newStack(t)

	req, err := http.NewRequest(http.MethodGet, s.bff+"/api/v1/resources/R001/status", nil) //nolint:noctx
	testutil.NoError(t, err, "build request")
	req.Header.Set("X-Tenant-ID", "local")
	req.Header.Set("X-Correlation-ID", "integration-test-correlation-id")

	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "request")
	defer func() { _ = resp.Body.Close() }()

	testutil.Equal(t, resp.Header.Get("X-Correlation-ID"), "integration-test-correlation-id",
		"a supplied correlation id is honoured and echoed")

	body, _ := io.ReadAll(resp.Body)
	var env envelope
	testutil.NoError(t, json.Unmarshal(body, &env), "decode")
	testutil.Equal(t, env.Meta.CorrelationID, "integration-test-correlation-id",
		"and appears in the envelope, so a support ticket quoting it can find the trace")
}

// TestIntegration_HostileCorrelationIDIsReplaced verifies REQ-API-006 and
// REQ-OBS-013: a correlation id ends up in logs and metric attributes, so an
// unacceptable one is replaced rather than propagated.
func TestIntegration_HostileCorrelationIDIsReplaced(t *testing.T) {
	s := newStack(t)

	req, err := http.NewRequest(http.MethodGet, s.bff+"/api/v1/resources/R001/status", nil) //nolint:noctx
	testutil.NoError(t, err, "build request")
	req.Header.Set("X-Tenant-ID", "local")
	req.Header.Set("X-Correlation-ID", "injected\"quote and spaces")

	resp, err := s.client.Do(req)
	testutil.NoError(t, err, "request")
	defer func() { _ = resp.Body.Close() }()

	got := resp.Header.Get("X-Correlation-ID")
	testutil.NotEqual(t, got, "injected\"quote and spaces", "the hostile value is not echoed back")
	testutil.NotEqual(t, got, "", "but a usable id is still issued")
}

// TestIntegration_AdminSurface verifies REQ-OBS-011 and REQ-OBS-012: probes and
// metrics live on their own listener and report what an operator needs.
func TestIntegration_AdminSurface(t *testing.T) {
	s := newStack(t)
	admin := env("BFF_ADMIN_URL", "http://localhost:9090")

	t.Run("liveness does not depend on the sources", func(t *testing.T) {
		s.chaos(t, s.opsAdmin, map[string]any{"unavailable": true})
		s.chaos(t, s.edsAdmin, map[string]any{"unavailable": true})
		defer s.resetChaos(t)

		resp, err := s.client.Get(admin + "/livez") //nolint:noctx
		testutil.NoError(t, err, "livez")
		defer func() { _ = resp.Body.Close() }()
		testutil.Equal(t, resp.StatusCode, http.StatusOK,
			"a source outage must not make the kubelet restart the BFF")
	})

	t.Run("readiness reports source state without failing", func(t *testing.T) {
		resp, err := s.client.Get(admin + "/readyz") //nolint:noctx
		testutil.NoError(t, err, "readyz")
		defer func() { _ = resp.Body.Close() }()
		testutil.Equal(t, resp.StatusCode, http.StatusOK, "status")

		var body struct {
			Status  string `json:"status"`
			Sources map[string]struct {
				Available bool   `json:"available"`
				State     string `json:"state"`
			} `json:"sources"`
		}
		raw, _ := io.ReadAll(resp.Body)
		testutil.NoError(t, json.Unmarshal(raw, &body), "decode")
		testutil.True(t, len(body.Sources) == 2, "both sources are reported")
	})

	t.Run("metrics are exposed in Prometheus format", func(t *testing.T) {
		// Generate some traffic first so the instruments have data.
		s.get(t, "local", "/api/v1/resources/R001/status")

		resp, err := s.client.Get(admin + "/metrics") //nolint:noctx
		testutil.NoError(t, err, "metrics")
		defer func() { _ = resp.Body.Close() }()
		testutil.Equal(t, resp.StatusCode, http.StatusOK, "status")

		raw, _ := io.ReadAll(resp.Body)
		text := string(raw)
		for _, name := range []string{
			"bff_request_total", "bff_request_latency", "routing_decision_total",
			"operational_source_latency",
		} {
			testutil.True(t, bytes.Contains(raw, []byte(name)),
				"metric %s must be exposed; got %d bytes of exposition", name, len(text))
		}
	})

	t.Run("the effective routing policy is inspectable", func(t *testing.T) {
		resp, err := s.client.Get(admin + "/config/routing?tenant=acme") //nolint:noctx
		testutil.NoError(t, err, "config/routing")
		defer func() { _ = resp.Body.Close() }()
		testutil.Equal(t, resp.StatusCode, http.StatusOK, "status")

		var body struct {
			Tenant       string                    `json:"tenant"`
			RequestTypes map[string]map[string]any `json:"request_types"`
		}
		raw, _ := io.ReadAll(resp.Body)
		testutil.NoError(t, json.Unmarshal(raw, &body), "decode")
		testutil.Equal(t, body.Tenant, "acme", "the tenant is echoed")

		rule := body.RequestTypes["resource_status"]
		testutil.True(t, rule != nil, "the request type is reported")
		testutil.Equal(t, rule["ttl"], "5s",
			"and shows the TENANT'S effective TTL, not the global default -- which is the whole "+
				"reason this endpoint exists during a routing incident")
	})
}

// TestIntegration_ConcurrentIdenticalRequests verifies REQ-EDGE-012 through the
// real stack: a burst for one resource must not become a burst on the sources.
func TestIntegration_ConcurrentIdenticalRequests(t *testing.T) {
	s := newStack(t)

	const idx = 21
	id, tenant := resourceID(idx), tenantOf(idx)

	const n = 30
	errCh := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, _ := s.get(t, tenant, "/api/v1/resources/"+id+"/details")
			errCh <- resp.StatusCode
		}()
	}
	for i := 0; i < n; i++ {
		code := <-errCh
		testutil.True(t, code == http.StatusOK || code == http.StatusPartialContent,
			"concurrent request %d returned %d", i, code)
	}
}

// TestIntegration_SchemaVersionMismatch verifies REQ-EDGE-017. It asserts both
// halves of the behaviour, because they look contradictory unless stated
// together: a source speaking an unsupported contract is refused where no other
// source can answer, and falls back where one can. In neither case is the
// mismatched data served as if it were understood.
func TestIntegration_SchemaVersionMismatch(t *testing.T) {
	s := newStack(t)

	const idx = 18
	id, tenant := resourceID(idx), tenantOf(idx)
	s.touch(t, id)

	s.chaos(t, s.opsAdmin, map[string]any{"schema_version": "ods.v99"})

	t.Run("refused where the operational source is the only one that can answer", func(t *testing.T) {
		// resource_configuration is configured fallback: none -- no other
		// source holds configuration.
		// Wait for the specific code rather than for any failure: an earlier
		// test may have left the operational circuit breaker open, and a
		// transient NO_SOURCE_AVAILABLE on the way back up is not what this
		// test is about.
		var doc errorDocument
		var resp *http.Response
		s.waitFor(t, 30*time.Second, "the schema mismatch to surface", func() bool {
			resp, doc = s.getError(t, tenant, "/api/v1/resources/"+id+"/configuration")
			return doc.Error.Code == "SCHEMA_VERSION_MISMATCH"
		})
		testutil.Equal(t, doc.Error.Code, "SCHEMA_VERSION_MISMATCH", "error code")
		testutil.Equal(t, resp.StatusCode, http.StatusBadGateway, "status")
		testutil.False(t, doc.Error.Retryable, "retrying an incompatible contract cannot help")
	})

	t.Run("falls back where another source can answer", func(t *testing.T) {
		// resource_read is configured fallback: execution. The execution source
		// cannot supply configuration, owner, metrics or topology, so the
		// answer is a 206: it is served, and it is honest about being
		// incomplete rather than looking whole.
		var env envelope
		var resp *http.Response
		s.waitFor(t, 30*time.Second, "the fallback to engage", func() bool {
			resp, env = s.getEnvelope(t, tenant, "/api/v1/resources/"+id)
			return env.Meta.RoutingDecision == "EXECUTION"
		})
		testutil.Equal(t, resp.StatusCode, http.StatusPartialContent,
			"a fallback answer missing operational-only fields is partial, not complete")
		testutil.Equal(t, env.Meta.RoutingRule, "fallback.primary_failed", "the call-time fallback fired")
		testutil.True(t, env.Meta.Degraded, "and the answer is marked degraded")
		testutil.True(t, env.Meta.Partial, "and partial")
	})
}
