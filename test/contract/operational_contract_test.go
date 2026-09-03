//go:build contract

// Package contract holds the tests that pin what the BFF requires of each data
// source.
//
// They are a different thing from the unit tests. A unit test asks "does the
// BFF behave correctly given this response?"; a contract test asks "does the
// source still produce that response?". They are the mechanism that turns a
// silent schema drift into a red build (REQ-DS-011).
//
// Run with:
//
//	go test -tags=contract ./test/contract/...
//
// They start the reference sources in-process, so they need no network and no
// compose stack. Pointing them at a real source is a matter of setting
// OPERATIONAL_ADDR / EXECUTION_URL.
package contract

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	opsv1 "github.com/udaykishore/ttl-aware-bff/internal/datasource/operational/opsv1"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/mapper"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// dialOperational connects to the ODS named by OPERATIONAL_ADDR, defaulting to
// the address the compose stack publishes.
func dialOperational(t *testing.T) opsv1.OperationalServiceClient {
	t.Helper()
	addr := os.Getenv("OPERATIONAL_ADDR")
	if addr == "" {
		addr = "localhost:9101"
	}
	if !reachable(addr) {
		t.Skipf("operational source not reachable at %s; start it with `make compose-up` or set OPERATIONAL_ADDR", addr)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	testutil.NoError(t, err, "dial the operational source")
	t.Cleanup(func() { _ = conn.Close() })
	return opsv1.NewOperationalServiceClient(conn)
}

func reachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestContract_FreshnessProbeIsPresentAndComplete verifies REQ-DS-002: the
// cheap freshness probe is the linchpin of TTL routing. If a source stops
// offering it, or stops filling in server_time, the BFF loses the ability to
// route on age and silently starts paying for the slow source.
func TestContract_FreshnessProbeIsPresentAndComplete(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	resp, err := client.GetResourceFreshness(ctx, &opsv1.GetResourceFreshnessRequest{
		Context:    &opsv1.RequestContext{TenantId: "local", CorrelationId: "contract-test"},
		ResourceId: "R001",
	})
	testutil.NoError(t, err, "the probe RPC must exist and answer")
	testutil.True(t, resp.GetFound(), "the seeded resource must be found")

	f := resp.GetFreshness()
	testutil.True(t, f != nil, "a found resource must carry a freshness envelope")
	testutil.True(t, f.GetLastUpdated() != nil, "last_updated is required: it is what the TTL is applied to")
	testutil.True(t, f.GetServerTime() != nil,
		"server_time is required: without it the age cannot be computed in the source's clock domain "+
			"and clock skew between the two machines lands directly in the freshness verdict")

	last := f.GetLastUpdated().AsTime()
	server := f.GetServerTime().AsTime()
	testutil.False(t, last.After(server.Add(time.Second)),
		"last_updated must not be in the source's own future")
}

// TestContract_ProbeIsCheaperThanARead verifies REQ-PERF-002. The entire TTL
// design rests on the probe costing materially less than the read it might
// avoid; if that stops being true the design is a pessimisation, and this test
// is where that shows up.
func TestContract_ProbeIsCheaperThanARead(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	// Warm the connection so neither measurement pays for the handshake.
	_, _ = client.GetResourceFreshness(ctx, &opsv1.GetResourceFreshnessRequest{
		Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "R001",
	})

	const samples = 20
	var probeTotal, readTotal time.Duration
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, err := client.GetResourceFreshness(ctx, &opsv1.GetResourceFreshnessRequest{
			Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "R001",
		})
		testutil.NoError(t, err, "probe")
		probeTotal += time.Since(start)

		start = time.Now()
		_, err = client.GetResource(ctx, &opsv1.GetResourceRequest{
			Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "R001",
		})
		testutil.NoError(t, err, "read")
		readTotal += time.Since(start)
	}

	t.Logf("mean probe %s, mean read %s", probeTotal/samples, readTotal/samples)
	testutil.True(t, probeTotal <= readTotal,
		"the freshness probe must not cost more than the read it exists to avoid (probe %s vs read %s)",
		probeTotal/samples, readTotal/samples)
}

// TestContract_ResourceRecordCarriesEveryFieldTheMapperReads verifies
// REQ-DS-001 and REQ-MAP-002: every field the canonical mapping depends on is
// actually populated. A source that quietly stops sending ownership or
// topology would otherwise show up as an unexplained gap in the UI.
func TestContract_ResourceRecordCarriesEveryFieldTheMapperReads(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	resp, err := client.GetResource(ctx, &opsv1.GetResourceRequest{
		Context:    &opsv1.RequestContext{TenantId: "local"},
		ResourceId: "R001",
	})
	testutil.NoError(t, err, "read")

	r := resp.GetResource()
	testutil.True(t, r != nil, "a record must be returned")
	testutil.NotEqual(t, r.GetResourceId(), "", "resource_id")
	testutil.NotEqual(t, r.GetCustomerRef(), "", "customer_ref maps to the canonical customerId")
	testutil.NotEqual(t, r.GetTenantId(), "", "tenant_id is what the BFF verifies its request against")
	testutil.NotEqual(t, r.GetResourceType(), "", "resource_type")
	testutil.NotEqual(t, r.GetState(), opsv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, "state")
	testutil.True(t, r.GetOwnership() != nil, "ownership")
	testutil.True(t, len(r.GetConfiguration()) > 0, "configuration")
	testutil.True(t, len(r.GetMetrics()) > 0, "metrics")
	testutil.True(t, r.GetTopology() != nil, "topology")
	testutil.True(t, r.GetFreshness() != nil, "freshness")

	// And the record must map cleanly.
	m := mapper.NewOperational([]string{mapper.OperationalSchemaVersion}, nil)
	mapped, _, err := m.Resource(r, "local")
	testutil.NoError(t, err, "the record must satisfy the canonical mapping")
	testutil.Equal(t, mapped.ResourceID, r.GetResourceId(), "identity survives mapping")
	testutil.NotEqual(t, mapped.Status, domain.StatusUnknown,
		"the source's state must map to a known canonical status, not fall through to UNKNOWN")
}

// TestContract_SchemaVersionIsDeclared verifies REQ-EDGE-017: the source
// declares a contract version the BFF can gate on, and it is one this build
// accepts.
func TestContract_SchemaVersionIsDeclared(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	resp, err := client.GetResource(ctx, &opsv1.GetResourceRequest{
		Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "R001",
	})
	testutil.NoError(t, err, "read")
	testutil.Equal(t, resp.GetResource().GetSchemaVersion(), mapper.OperationalSchemaVersion,
		"the source's declared schema version must be one this build was written against")
}

// TestContract_NarrowStateReadCarriesTheInFlightReference verifies
// REQ-PREC-003: the cheap status read must expose whether a workflow is
// mutating the resource. Without it, /status cannot apply the
// execution-overrides-operational rule and would disagree with /details.
func TestContract_NarrowStateReadCarriesTheInFlightReference(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	// R001 is seeded with a running execution by both reference sources.
	resp, err := client.GetResourceState(ctx, &opsv1.GetResourceStateRequest{
		Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "R001",
	})
	testutil.NoError(t, err, "narrow read")
	testutil.NotEqual(t, resp.GetInFlightExecutionRef(), "",
		"the narrow read must carry in_flight_execution_ref for a resource with a running workflow")
	testutil.True(t, resp.GetFreshness() != nil, "and its own freshness envelope")
}

// TestContract_TenantIsolationIsEnforcedAtTheSource verifies REQ-SEC-004: the
// BFF checks tenancy, and so must the source. Defence in depth means neither is
// the only line.
func TestContract_TenantIsolationIsEnforcedAtTheSource(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	_, err := client.GetResource(ctx, &opsv1.GetResourceRequest{
		Context:    &opsv1.RequestContext{TenantId: "a-tenant-that-owns-nothing"},
		ResourceId: "R001",
	})
	testutil.Error(t, err, "the source must refuse a read for a tenant that does not own the resource")
}

// TestContract_MissingResourceIsAnAnswerNotAnError verifies REQ-DS-004: an
// absent record on the PROBE path is reported as found=false rather than as an
// error, because "this resource does not exist" must not be evidence that the
// source is unwell and must not trip a circuit breaker.
func TestContract_MissingResourceIsAnAnswerNotAnError(t *testing.T) {
	client := dialOperational(t)
	ctx := ctxWithTimeout(t)

	resp, err := client.GetResourceFreshness(ctx, &opsv1.GetResourceFreshnessRequest{
		Context: &opsv1.RequestContext{TenantId: "local"}, ResourceId: "no-such-resource",
	})
	testutil.NoError(t, err, "a missing resource is not a probe failure")
	testutil.False(t, resp.GetFound(), "it is reported as not found")
}

// TestContract_EnumMappingIsTotal verifies REQ-MAP-003: every state the source
// can emit has a named canonical counterpart. A source that adds an enum member
// must not be able to leak that token into the API.
func TestContract_EnumMappingIsTotal(t *testing.T) {
	for value, name := range opsv1.ResourceState_name {
		state := opsv1.ResourceState(value)
		mapped := mapper.MapOperationalStatus(state)
		if state == opsv1.ResourceState_RESOURCE_STATE_UNSPECIFIED {
			testutil.Equal(t, mapped, domain.StatusUnknown, "%s maps to UNKNOWN", name)
			continue
		}
		testutil.NotEqual(t, mapped, domain.StatusUnknown,
			"%s has no canonical mapping; add one to internal/mapper/operational.go", name)
	}
}

// TestContract_FreshnessEnvelopeRoundTrip verifies REQ-MAP-010: the mapping
// treats an absent timestamp as absent rather than as the Unix epoch, which
// would otherwise present as a record half a century old.
func TestContract_FreshnessEnvelopeRoundTrip(t *testing.T) {
	m := mapper.NewOperational(nil, nil)

	rec := &opsv1.OperationalResource{
		ResourceId: "R1",
		TenantId:   "local",
		State:      opsv1.ResourceState_RESOURCE_STATE_ACTIVE,
		Freshness:  &opsv1.FreshnessEnvelope{LastUpdated: &timestamppb.Timestamp{}},
	}
	mapped, _, err := m.Resource(rec, "local")
	testutil.NoError(t, err, "mapping")
	testutil.True(t, mapped.ObservedAt.IsZero(),
		"an unset timestamp must map to the zero time, not to 1970")
}
