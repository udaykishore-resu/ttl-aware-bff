package mapper

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	opsv1 "github.com/udaykishore/ttl-aware-bff/internal/datasource/operational/opsv1"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// ---------------------------------------------------------------------------
// Enum totality
// ---------------------------------------------------------------------------

// TestMapOperationalStatus_IsTotal verifies REQ-MAP-002 and REQ-MAP-003: every
// declared ODS ResourceState has a named canonical counterpart. The enumeration
// comes from the generated ResourceState_name map rather than from a hand-kept
// list, so adding a member to the proto breaks this test instead of silently
// falling through to UNKNOWN.
func TestMapOperationalStatus_IsTotal(t *testing.T) {
	t.Parallel()

	testutil.True(t, len(opsv1.ResourceState_name) > 0, "the generated enum table must not be empty")

	for num, name := range opsv1.ResourceState_name {
		state := opsv1.ResourceState(num)
		got := MapOperationalStatus(state)

		// The zero value of ResourceStatus is "", so a missing table entry that
		// somehow returned the zero value would be a silent gap.
		testutil.NotEqual(t, got, domain.ResourceStatus(""),
			"%s (%d) must map to a named canonical status, not the zero value", name, num)

		if name == "RESOURCE_STATE_UNSPECIFIED" {
			testutil.Equal(t, got, domain.StatusUnknown, "%s must map to UNKNOWN", name)
			continue
		}
		testutil.NotEqual(t, got, domain.StatusUnknown,
			"%s (%d) must have an explicit mapping, not fall through to UNKNOWN", name, num)
	}

	// Every declared state is covered by the explicit table, not by the
	// fall-through branch.
	testutil.Equal(t, len(operationalStatus), len(opsv1.ResourceState_name),
		"the mapping table must have exactly one entry per declared enum member")
}

// TestMapOperationalStatus_Table verifies REQ-MAP-003: the documented
// ResourceState to ResourceStatus mapping, value by value.
func TestMapOperationalStatus_Table(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state opsv1.ResourceState
		want  domain.ResourceStatus
	}{
		{opsv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, domain.StatusUnknown},
		{opsv1.ResourceState_RESOURCE_STATE_PROVISIONING, domain.StatusPending},
		{opsv1.ResourceState_RESOURCE_STATE_ACTIVE, domain.StatusActive},
		{opsv1.ResourceState_RESOURCE_STATE_SUSPENDED, domain.StatusSuspended},
		{opsv1.ResourceState_RESOURCE_STATE_DEGRADED, domain.StatusDegraded},
		{opsv1.ResourceState_RESOURCE_STATE_TERMINATING, domain.StatusTerminating},
		{opsv1.ResourceState_RESOURCE_STATE_TERMINATED, domain.StatusTerminated},
		{opsv1.ResourceState_RESOURCE_STATE_ERROR, domain.StatusError},
	}
	for _, tc := range cases {
		t.Run(opsv1.ResourceState_name[int32(tc.state)], func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, MapOperationalStatus(tc.state), tc.want, "state %v", tc.state)
		})
	}

	// A value the source invented is never guessed at: it becomes UNKNOWN.
	testutil.Equal(t, MapOperationalStatus(opsv1.ResourceState(9999)), domain.StatusUnknown,
		"an undeclared enum number must map to UNKNOWN, not to a plausible neighbour")
	testutil.Equal(t, MapOperationalStatus(opsv1.ResourceState(-1)), domain.StatusUnknown,
		"a negative enum number must map to UNKNOWN")
}

// ---------------------------------------------------------------------------
// Resource()
// ---------------------------------------------------------------------------

// TestOperational_Resource_Rejects verifies REQ-MAP-008, REQ-MT-004 and
// REQ-EDGE-016/020: a payload that cannot be trusted is refused with the right
// taxonomy code rather than coerced into a canonical record.
func TestOperational_Resource_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		src    *opsv1.OperationalResource
		tenant string
		want   errs.Code
		detail string
	}{
		{
			name:   "nil record",
			src:    nil,
			tenant: "tenant-a",
			want:   errs.CodeUpstreamInvalidPayload,
			detail: "an empty record from the source is not an empty resource",
		},
		{
			name:   "foreign tenant",
			src:    &opsv1.OperationalResource{ResourceId: "res-1", TenantId: "tenant-b"},
			tenant: "tenant-a",
			want:   errs.CodeTenantMismatch,
			detail: "a record belonging to another tenant is a hard failure (REQ-MT-004, REQ-EDGE-016)",
		},
		{
			name:   "missing resource id",
			src:    &opsv1.OperationalResource{TenantId: "tenant-a"},
			tenant: "tenant-a",
			want:   errs.CodeUpstreamInvalidPayload,
			detail: "a record without its identity cannot be mapped",
		},
		{
			name:   "unknown schema version",
			src:    &opsv1.OperationalResource{ResourceId: "res-1", TenantId: "tenant-a", SchemaVersion: "ods.v9"},
			tenant: "tenant-a",
			want:   errs.CodeSchemaVersionMismatch,
			detail: "a schema this build was not written against is terminal (REQ-EDGE-017)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewOperational([]string{OperationalSchemaVersion}, nil)
			res, fr, err := m.Resource(tc.src, tc.tenant)
			testutil.Error(t, err, "%s", tc.detail)
			testutil.ErrCode(t, err, tc.want, "%s", tc.detail)
			testutil.True(t, res == nil, "no resource is returned on failure")
			testutil.Equal(t, fr, domain.Freshness{}, "no freshness is invented on failure")
		})
	}
}

// TestOperational_Resource_TenantMismatchOrdering verifies REQ-MT-004: the
// tenant check is only skipped when there is nothing to compare, and a matching
// tenant is accepted.
func TestOperational_Resource_TenantMismatchOrdering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		recordOwner string
		requested   string
		wantErr     bool
		wantTenant  string
	}{
		{name: "matching tenants", recordOwner: "tenant-a", requested: "tenant-a", wantTenant: "tenant-a"},
		{name: "mismatched tenants", recordOwner: "tenant-b", requested: "tenant-a", wantErr: true},
		{name: "record omits its tenant", recordOwner: "", requested: "tenant-a", wantTenant: "tenant-a"},
		{name: "caller omits the tenant", recordOwner: "tenant-b", requested: "", wantTenant: "tenant-b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewOperational(nil, nil)
			out, _, err := m.Resource(&opsv1.OperationalResource{
				ResourceId: "res-1",
				TenantId:   tc.recordOwner,
			}, tc.requested)
			if tc.wantErr {
				testutil.ErrCode(t, err, errs.CodeTenantMismatch, "cross-tenant records are refused")
				return
			}
			testutil.NoError(t, err, "mapping should succeed")
			testutil.Equal(t, out.TenantID, tc.wantTenant, "resolved tenant id")
		})
	}
}

// TestOperational_Resource_FieldMapping verifies REQ-MAP-005: every source
// field lands in its declared canonical home, including the renames
// (customer_ref -> customerId, state -> status, substate -> subState,
// operational_metadata -> metadata, in_flight_execution_ref ->
// inFlightExecutionId).
func TestOperational_Resource_FieldMapping(t *testing.T) {
	t.Parallel()

	observed := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	sampled := time.Date(2024, 3, 1, 11, 59, 0, 0, time.UTC)

	src := &opsv1.OperationalResource{
		ResourceId:   "res-1",
		CustomerRef:  "cust-42",
		TenantId:     "tenant-a",
		ResourceType: "database",
		State:        opsv1.ResourceState_RESOURCE_STATE_DEGRADED,
		Substate:     "REBALANCING",
		Ownership: &opsv1.OwnershipRecord{
			OwnerId:    "team-platform",
			OwnerType:  "team",
			OwnerEmail: "platform@example.invalid",
			CostCentre: "CC-1000",
		},
		Configuration: map[string]string{"tier": "gold", "replicas": "3"},
		Metrics: []*opsv1.MetricSample{
			{Name: "cpu", Value: 0.75, Unit: "ratio", SampledAt: timestamppb.New(sampled)},
			// A nameless sample is dropped rather than mapped to an empty metric.
			{Name: "", Value: 1},
			nil,
		},
		Topology: &opsv1.TopologyRecord{
			Region:         "eu-west-1",
			Zone:           "eu-west-1a",
			Cluster:        "cluster-7",
			UpstreamRefs:   []string{"res-0"},
			DownstreamRefs: []string{"res-2", "res-3"},
		},
		OperationalMetadata:  map[string]string{"poller": "eu-1"},
		Labels:               map[string]string{"env": "prod"},
		Freshness:            &opsv1.FreshnessEnvelope{LastUpdated: timestamppb.New(observed), Version: 17},
		InFlightExecutionRef: "exec-9",
		SchemaVersion:        OperationalSchemaVersion,
	}

	m := NewOperational([]string{OperationalSchemaVersion}, nil)
	got, fr, err := m.Resource(src, "tenant-a")
	testutil.NoError(t, err, "mapping a complete record")

	testutil.Equal(t, got.TenantID, "tenant-a", "tenant_id -> tenantId")
	testutil.Equal(t, got.ResourceID, "res-1", "resource_id -> resourceId")
	testutil.Equal(t, got.CustomerID, "cust-42", "customer_ref -> customerId (rename)")
	testutil.Equal(t, got.Type, "database", "resource_type -> type (rename)")
	testutil.Equal(t, got.Status, domain.StatusDegraded, "state -> status (rename)")
	testutil.Equal(t, got.SubState, "REBALANCING", "substate -> subState (rename)")
	testutil.Equal(t, got.InFlightExecutionID, "exec-9", "in_flight_execution_ref -> inFlightExecutionId (rename)")
	testutil.Equal(t, got.Metadata, map[string]string{"poller": "eu-1"}, "operational_metadata -> metadata (rename)")
	testutil.Equal(t, got.Labels, map[string]string{"env": "prod"}, "labels")
	testutil.Equal(t, got.Configuration, map[string]string{"tier": "gold", "replicas": "3"}, "configuration")
	testutil.Equal(t, got.SchemaVersion, OperationalSchemaVersion, "schema version is carried")

	testutil.Equal(t, got.Owner, &domain.Owner{
		ID:         "team-platform",
		Type:       "team",
		Email:      "platform@example.invalid",
		CostCentre: "CC-1000",
	}, "ownership -> owner (rename)")

	testutil.Equal(t, got.Topology, &domain.Topology{
		Region:     "eu-west-1",
		Zone:       "eu-west-1a",
		Cluster:    "cluster-7",
		Upstream:   []string{"res-0"},
		Downstream: []string{"res-2", "res-3"},
	}, "topology.upstream_refs -> topology.upstream (rename)")

	// REQ-MAP-009: the value is a float64 and the unit is carried verbatim.
	testutil.Equal(t, len(got.Metrics), 1, "a nameless and a nil sample are dropped")
	testutil.Equal(t, got.Metrics[0], domain.Metric{
		Name: "cpu", Value: 0.75, Unit: "ratio", SampledAt: sampled,
	}, "metric sample")

	testutil.Equal(t, got.ObservedAt, observed, "freshness.last_updated -> observedAt")
	testutil.Equal(t, fr.State, domain.FreshnessUnknown, "the mapper translates freshness, it does not evaluate it")
	testutil.Equal(t, fr.ObservedAt, observed, "freshness observedAt")
	testutil.Equal(t, fr.Source, domain.SourceOperational, "freshness source")
	testutil.Equal(t, fr.Version, uint64(17), "freshness version")
	testutil.Equal(t, fr.Age, time.Duration(0), "the mapper must not compute an age")
	testutil.Equal(t, fr.TTL, time.Duration(0), "the mapper must not apply a TTL")
}

// TestOperational_Resource_CopiesInsteadOfAliasing verifies REQ-MAP-007: the
// mapper produces a value the caller owns, so mutating the canonical record
// cannot reach back into the source payload.
func TestOperational_Resource_CopiesInsteadOfAliasing(t *testing.T) {
	t.Parallel()

	src := &opsv1.OperationalResource{
		ResourceId:    "res-1",
		Configuration: map[string]string{"tier": "gold"},
		Topology:      &opsv1.TopologyRecord{Region: "eu-west-1", UpstreamRefs: []string{"res-0"}},
	}
	m := NewOperational(nil, nil)
	got, _, err := m.Resource(src, "tenant-a")
	testutil.NoError(t, err, "mapping")

	got.Configuration["tier"] = "bronze"
	got.Topology.Upstream[0] = "mutated"

	testutil.Equal(t, src.Configuration["tier"], "gold", "the source map must not be aliased")
	testutil.Equal(t, src.Topology.UpstreamRefs[0], "res-0", "the source slice must not be aliased")
}

// TestOperational_Resource_OmitsEmptyStructures verifies REQ-MAP-006: a field
// the source did not populate stays empty; the mapper never invents a
// placeholder for it.
func TestOperational_Resource_OmitsEmptyStructures(t *testing.T) {
	t.Parallel()

	m := NewOperational(nil, nil)
	got, _, err := m.Resource(&opsv1.OperationalResource{
		ResourceId: "res-1",
		// An ownership record with no identity at all is not an owner.
		Ownership: &opsv1.OwnershipRecord{OwnerType: "team"},
		// A topology record with no placement is not a topology.
		Topology: &opsv1.TopologyRecord{Zone: "eu-west-1a"},
		Metrics:  []*opsv1.MetricSample{},
	}, "tenant-a")
	testutil.NoError(t, err, "mapping a sparse record")

	testutil.True(t, got.Owner == nil, "an ownership record with no id or email yields no owner")
	testutil.True(t, got.Topology == nil, "a topology record with no region, cluster or upstream yields no topology")
	testutil.True(t, got.Metrics == nil, "an empty metric list yields no metrics")
	testutil.True(t, got.Configuration == nil, "an absent configuration map yields nil, not an empty map")
	testutil.True(t, got.Labels == nil, "an absent label map yields nil, not an empty map")
	testutil.True(t, got.Metadata == nil, "an absent metadata map yields nil, not an empty map")
}

// TestOperational_Resource_PartiallyPopulated verifies REQ-EDGE-006: a record
// the source only half-filled maps without error, and Completeness reports how
// much of it arrived so the aggregator can warn.
func TestOperational_Resource_PartiallyPopulated(t *testing.T) {
	t.Parallel()

	m := NewOperational(nil, nil)

	t.Run("identity only", func(t *testing.T) {
		t.Parallel()
		got, _, err := m.Resource(&opsv1.OperationalResource{ResourceId: "res-1"}, "tenant-a")
		testutil.NoError(t, err, "an identity-only record is valid, just incomplete")
		// resourceId present; status maps to UNKNOWN which does not count.
		testutil.Equal(t, got.Completeness(), 1.0/8.0, "only the identity arrived")
		testutil.False(t, got.IsZero(), "a record with an id and no status is still not zero")
	})

	t.Run("half populated", func(t *testing.T) {
		t.Parallel()
		got, _, err := m.Resource(&opsv1.OperationalResource{
			ResourceId:    "res-1",
			CustomerRef:   "cust-42",
			ResourceType:  "database",
			State:         opsv1.ResourceState_RESOURCE_STATE_ACTIVE,
			Configuration: nil,
			Metrics:       nil,
		}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, got.Completeness(), 4.0/8.0, "four of the eight core fields arrived")
	})

	t.Run("fully populated", func(t *testing.T) {
		t.Parallel()
		got, _, err := m.Resource(&opsv1.OperationalResource{
			ResourceId:    "res-1",
			CustomerRef:   "cust-42",
			ResourceType:  "database",
			State:         opsv1.ResourceState_RESOURCE_STATE_ACTIVE,
			Ownership:     &opsv1.OwnershipRecord{OwnerId: "team-platform"},
			Configuration: map[string]string{"tier": "gold"},
			Metrics:       []*opsv1.MetricSample{{Name: "cpu", Value: 1}},
			Topology:      &opsv1.TopologyRecord{Region: "eu-west-1"},
		}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, got.Completeness(), 1.0, "every core field arrived")
	})
}

// ---------------------------------------------------------------------------
// Schema version gating
// ---------------------------------------------------------------------------

// TestOperational_SchemaVersion verifies REQ-EDGE-017: an unset schema version
// is tolerated (a source that has not adopted the field yet must not break the
// BFF), a known one is accepted, and an unknown one is terminal and fires the
// mismatch callback exactly once.
func TestOperational_SchemaVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		accepted []string
		declared string
		wantErr  bool
		wantSeen []string
	}{
		{name: "unset is accepted", accepted: []string{OperationalSchemaVersion}, declared: ""},
		{name: "known is accepted", accepted: []string{OperationalSchemaVersion}, declared: OperationalSchemaVersion},
		{name: "one of several", accepted: []string{"ods.v0", "ods.v1"}, declared: "ods.v0"},
		{name: "no gate accepts anything", accepted: nil, declared: "ods.v99"},
		{name: "empty gate accepts anything", accepted: []string{}, declared: "ods.v99"},
		{
			name:     "unknown is refused",
			accepted: []string{OperationalSchemaVersion},
			declared: "ods.v2",
			wantErr:  true,
			wantSeen: []string{"ods.v2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen []string
			m := NewOperational(tc.accepted, func(got string) { seen = append(seen, got) })

			_, _, err := m.Resource(&opsv1.OperationalResource{
				ResourceId:    "res-1",
				TenantId:      "tenant-a",
				SchemaVersion: tc.declared,
			}, "tenant-a")

			if tc.wantErr {
				testutil.ErrCode(t, err, errs.CodeSchemaVersionMismatch, "an unknown schema version is terminal")
				e, ok := errs.As(err)
				testutil.True(t, ok, "the error carries the taxonomy type")
				testutil.False(t, e.Retryable, "a schema mismatch is not retryable")
				testutil.Equal(t, e.Source, string(domain.SourceOperational), "the failing source is named")
			} else {
				testutil.NoError(t, err, "schema version %q should be accepted", tc.declared)
			}
			testutil.Equal(t, seen, tc.wantSeen, "mismatch callback invocations")
		})
	}
}

// TestNewOperational_NilCallbackIsSafe verifies that the mapper is usable
// without a mismatch hook (REQ-MAP-007: mappers do not log or observe).
func TestNewOperational_NilCallbackIsSafe(t *testing.T) {
	t.Parallel()

	m := NewOperational([]string{OperationalSchemaVersion}, nil)
	_, _, err := m.Resource(&opsv1.OperationalResource{ResourceId: "res-1", SchemaVersion: "ods.v7"}, "tenant-a")
	testutil.ErrCode(t, err, errs.CodeSchemaVersionMismatch, "the mismatch is still reported without a hook")
}

// ---------------------------------------------------------------------------
// State()
// ---------------------------------------------------------------------------

// TestOperational_State verifies REQ-MAP-003 and REQ-PREC-003: the narrow
// status read maps to the same canonical vocabulary as the full read and
// carries in_flight_execution_ref, which is what lets /status and /details
// agree during a workflow.
func TestOperational_State(t *testing.T) {
	t.Parallel()

	observed := time.Date(2024, 5, 4, 9, 30, 0, 0, time.UTC)
	m := NewOperational([]string{OperationalSchemaVersion}, nil)

	t.Run("maps the narrow response", func(t *testing.T) {
		t.Parallel()
		got, fr, err := m.State(&opsv1.GetResourceStateResponse{
			ResourceId:           "res-1",
			State:                opsv1.ResourceState_RESOURCE_STATE_TERMINATING,
			Substate:             "DRAINING",
			InFlightExecutionRef: "exec-9",
			Freshness:            &opsv1.FreshnessEnvelope{LastUpdated: timestamppb.New(observed), Version: 3},
		}, "tenant-a")
		testutil.NoError(t, err, "mapping the narrow response")

		testutil.Equal(t, got.TenantID, "tenant-a", "the requested tenant is stamped on the record")
		testutil.Equal(t, got.ResourceID, "res-1", "resource_id")
		testutil.Equal(t, got.Status, domain.StatusTerminating, "state -> status")
		testutil.Equal(t, got.SubState, "DRAINING", "substate -> subState")
		testutil.Equal(t, got.InFlightExecutionID, "exec-9",
			"in_flight_execution_ref must survive the narrow read (REQ-PREC-003)")
		testutil.Equal(t, got.ObservedAt, observed, "observedAt")

		testutil.Equal(t, fr.State, domain.FreshnessUnknown, "freshness is translated, not evaluated")
		testutil.Equal(t, fr.ObservedAt, observed, "freshness observedAt")
		testutil.Equal(t, fr.Source, domain.SourceOperational, "freshness source")
		testutil.Equal(t, fr.Version, uint64(3), "freshness version")

		// The narrow projection deliberately carries nothing else.
		testutil.True(t, got.Configuration == nil, "the narrow read carries no configuration")
		testutil.True(t, got.Metrics == nil, "the narrow read carries no metrics")
		testutil.True(t, got.Owner == nil, "the narrow read carries no owner")
	})

	t.Run("rejects an empty response", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			resp *opsv1.GetResourceStateResponse
		}{
			{"nil", nil},
			{"missing resource id", &opsv1.GetResourceStateResponse{State: opsv1.ResourceState_RESOURCE_STATE_ACTIVE}},
		} {
			_, fr, err := m.State(tc.resp, "tenant-a")
			testutil.ErrCode(t, err, errs.CodeUpstreamInvalidPayload, "%s must be refused", tc.name)
			testutil.Equal(t, fr, domain.Freshness{}, "no freshness is invented for %s", tc.name)
		}
	})

	t.Run("absent freshness envelope", func(t *testing.T) {
		t.Parallel()
		got, fr, err := m.State(&opsv1.GetResourceStateResponse{ResourceId: "res-1"}, "tenant-a")
		testutil.NoError(t, err, "a response without a freshness envelope is still usable")
		testutil.Equal(t, fr.State, domain.FreshnessUnknown, "freshness state")
		testutil.Equal(t, fr.Source, domain.SourceOperational, "freshness source is still named")
		testutil.True(t, fr.ObservedAt.IsZero(), "no observation instant is invented")
		testutil.True(t, got.ObservedAt.IsZero(), "no observation instant is invented on the record either")
	})
}

// ---------------------------------------------------------------------------
// Timestamps
// ---------------------------------------------------------------------------

// TestOperational_Timestamps verifies REQ-MAP-010: an absent or unusable source
// timestamp becomes the zero time.Time and is treated as absent, never as the
// Unix epoch.
func TestOperational_Timestamps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   *timestamppb.Timestamp
		want time.Time
	}{
		{name: "nil", in: nil, want: time.Time{}},
		{name: "underflow", in: &timestamppb.Timestamp{Seconds: -100000000000}, want: time.Time{}},
		{name: "overflow", in: &timestamppb.Timestamp{Seconds: 300000000000}, want: time.Time{}},
		{name: "negative nanos", in: &timestamppb.Timestamp{Seconds: 1700000000, Nanos: -1}, want: time.Time{}},
		{name: "nanos out of range", in: &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 1_000_000_000}, want: time.Time{}},
		{
			name: "valid instant",
			in:   timestamppb.New(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)),
			want: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, ts(tc.in), tc.want, "ts(%v)", tc.in)
		})
	}

	t.Run("an unusable timestamp is not the epoch", func(t *testing.T) {
		t.Parallel()
		m := NewOperational(nil, nil)
		got, fr, err := m.Resource(&opsv1.OperationalResource{
			ResourceId: "res-1",
			Freshness:  &opsv1.FreshnessEnvelope{LastUpdated: &timestamppb.Timestamp{Seconds: -100000000000}},
		}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.True(t, got.ObservedAt.IsZero(), "observedAt must be the zero time")
		testutil.NotEqual(t, got.ObservedAt, time.Unix(0, 0).UTC(), "observedAt must not be the Unix epoch")
		testutil.True(t, fr.ObservedAt.IsZero(), "freshness observedAt must be the zero time")
	})

	t.Run("a metric sample with no timestamp", func(t *testing.T) {
		t.Parallel()
		m := NewOperational(nil, nil)
		got, _, err := m.Resource(&opsv1.OperationalResource{
			ResourceId: "res-1",
			Metrics:    []*opsv1.MetricSample{{Name: "cpu", Value: 0.5, Unit: "ratio"}},
		}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.True(t, got.Metrics[0].SampledAt.IsZero(), "an unsampled metric carries the zero time")
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// TestOperationalHelpers verifies the small shared helpers behave as the
// mapping rules assume (REQ-MAP-006).
func TestOperationalHelpers(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, firstNonEmpty("", "", "c"), "c", "firstNonEmpty picks the first populated value")
	testutil.Equal(t, firstNonEmpty("a", "b"), "a", "firstNonEmpty prefers the earlier value")
	testutil.Equal(t, firstNonEmpty(), "", "firstNonEmpty of nothing is empty")
	testutil.Equal(t, firstNonEmpty("", ""), "", "firstNonEmpty of empties is empty")

	testutil.True(t, copyMap(nil) == nil, "copying an absent map yields nil")
	testutil.True(t, copyMap(map[string]string{}) == nil, "copying an empty map yields nil")
	testutil.Equal(t, copyMap(map[string]string{"a": "b"}), map[string]string{"a": "b"}, "copyMap copies")

	testutil.True(t, copySlice(nil) == nil, "copying an absent slice yields nil")
	testutil.True(t, copySlice([]string{}) == nil, "copying an empty slice yields nil")
	testutil.Equal(t, copySlice([]string{"a"}), []string{"a"}, "copySlice copies")
}
