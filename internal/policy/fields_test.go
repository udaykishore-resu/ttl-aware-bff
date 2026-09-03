package policy

import (
	"sort"
	"testing"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// catalog builds a catalogue over an explicit supplier table.
func catalog(fields map[string][]string) *FieldCatalog {
	return NewFieldCatalog(config.PrecedenceConfig{Fields: fields})
}

// TestNewFieldCatalog_FromConfiguration verifies REQ-PREC-001 and REQ-CLS-003:
// the catalogue is derived from the same precedence configuration the policy
// uses, so "which source can supply this field?" and "which source wins?" can
// never disagree. Entries that name no routable source are dropped.
func TestNewFieldCatalog_FromConfiguration(t *testing.T) {
	t.Parallel()

	c := NewFieldCatalog(config.Default().Precedence)

	cases := map[string][]domain.SourceKind{
		FieldStatus:           {domain.SourceOperational, domain.SourceExecution},
		FieldSubState:         {domain.SourceOperational},
		FieldConfiguration:    {domain.SourceOperational},
		FieldCustomerID:       {domain.SourceOperational, domain.SourceExecution},
		FieldLatestExecution:  {domain.SourceExecution},
		FieldExecutionHistory: {domain.SourceExecution},
	}
	for field, want := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, c.Suppliers(field), want, "suppliers of %s", field)
		})
	}

	t.Run("unroutable sources are dropped", func(t *testing.T) {
		t.Parallel()
		d := catalog(map[string][]string{FieldStatus: {"none", "operational", "not-a-source"}})
		testutil.Equal(t, d.Suppliers(FieldStatus), []domain.SourceKind{domain.SourceOperational},
			"only sources the router can actually call survive")
	})
}

// TestSuppliers_UnknownFieldHasNone verifies REQ-CLS-003: an unmodelled field
// has no supplier. The router reads that as "no constraint" and the request
// validator reads it as "reject the caller's projection"; both need the empty
// answer rather than a guess.
func TestSuppliers_UnknownFieldHasNone(t *testing.T) {
	t.Parallel()

	c := NewFieldCatalog(config.Default().Precedence)
	testutil.Equal(t, len(c.Suppliers("noSuchField")), 0, "an unknown field has no supplier")
	testutil.Equal(t, len(c.Suppliers("")), 0, "and neither has the empty field name")
}

// TestSourcesFor_UsesMostAuthoritativeSupplier verifies REQ-RT-004: the set of
// sources a request needs is built from each field's *first* supplier. Taking
// the union of every supplier instead would make a request for `status` claim
// it needs both sources, and the TTL fast path -- the reason this service
// exists -- would never fire.
func TestSourcesFor_UsesMostAuthoritativeSupplier(t *testing.T) {
	t.Parallel()

	c := NewFieldCatalog(config.Default().Precedence)

	cases := []struct {
		name   string
		fields []string
		want   map[domain.SourceKind]bool
	}{
		{
			"a contested field counts only towards its first supplier",
			[]string{FieldStatus},
			map[domain.SourceKind]bool{domain.SourceOperational: true},
		},
		{
			"operational-only fields",
			[]string{FieldConfiguration, FieldMetrics, FieldTopology},
			map[domain.SourceKind]bool{domain.SourceOperational: true},
		},
		{
			"execution-only fields",
			[]string{FieldExecutionHistory, FieldLatestExecution},
			map[domain.SourceKind]bool{domain.SourceExecution: true},
		},
		{
			"a mix genuinely needs both",
			[]string{FieldConfiguration, FieldExecutionHistory},
			map[domain.SourceKind]bool{domain.SourceOperational: true, domain.SourceExecution: true},
		},
		{
			"unknown fields contribute nothing",
			[]string{"noSuchField"},
			map[domain.SourceKind]bool{},
		},
		{
			"no fields at all",
			nil,
			map[domain.SourceKind]bool{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, c.SourcesFor(tc.fields), tc.want, "sources for %v", tc.fields)
		})
	}
}

// TestExclusiveTo verifies REQ-RT-003 and REQ-RT-004: a request may be pinned
// to one source only when *every* requested field can come from that source and
// from no other. This is the check that lets a history request skip the
// operational source entirely, so it has to be strict in both directions.
func TestExclusiveTo(t *testing.T) {
	t.Parallel()

	c := NewFieldCatalog(config.Default().Precedence)

	cases := []struct {
		name       string
		fields     []string
		wantSource domain.SourceKind
		wantOK     bool
	}{
		{
			"every field operational-only",
			[]string{FieldConfiguration, FieldMetrics, FieldTopology, FieldOwner},
			domain.SourceOperational, true,
		},
		{
			"every field execution-only",
			[]string{FieldExecutionHistory, FieldLatestExecution, FieldWorkflowSteps},
			domain.SourceExecution, true,
		},
		{
			"fields from both sources",
			[]string{FieldConfiguration, FieldExecutionHistory},
			domain.SourceNone, false,
		},
		{
			// status has two suppliers, so no single source can be proven
			// sufficient -- even though its first supplier matches the others.
			"a field with two suppliers defeats exclusivity",
			[]string{FieldConfiguration, FieldStatus},
			domain.SourceNone, false,
		},
		{
			"the contested field alone",
			[]string{FieldStatus},
			domain.SourceNone, false,
		},
		{
			"an empty field list proves nothing",
			nil,
			domain.SourceNone, false,
		},
		{
			"only unknown fields prove nothing",
			[]string{"noSuchField", "alsoUnknown"},
			domain.SourceNone, false,
		},
		{
			// An unknown field carries no constraint, so it neither pins nor
			// unpins the decision; the known fields still settle it.
			"unknown fields alongside known ones are ignored",
			[]string{"noSuchField", FieldConfiguration},
			domain.SourceOperational, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := c.ExclusiveTo(tc.fields)
			testutil.Equal(t, ok, tc.wantOK, "exclusivity verdict for %v", tc.fields)
			testutil.Equal(t, got, tc.wantSource, "exclusive source for %v", tc.fields)
		})
	}
}

// TestKnownFields_IsSorted verifies REQ-CLS-003: the validator reports the
// known fields back to a caller who asked for something unmodelled, so the list
// is ordered rather than being whatever the map iterator produced this time.
func TestKnownFields_IsSorted(t *testing.T) {
	t.Parallel()

	c := NewFieldCatalog(config.Default().Precedence)
	got := c.KnownFields()

	testutil.True(t, len(got) > 0, "the default catalogue is not empty")
	testutil.True(t, sort.StringsAreSorted(got), "known fields must be sorted, got %v", got)

	// Called twice, the same order comes back: the sort is not a coincidence of
	// one particular map walk.
	testutil.Equal(t, c.KnownFields(), got, "the listing is stable across calls")

	seen := map[string]bool{}
	for _, f := range got {
		testutil.False(t, seen[f], "field %s is listed twice", f)
		seen[f] = true
	}
	testutil.True(t, seen[FieldStatus] && seen[FieldExecutionHistory],
		"the listing covers both sources' fields, got %v", got)

	t.Run("an empty catalogue lists nothing", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, len(catalog(nil).KnownFields()), 0, "no configuration, no known fields")
	})
}

// TestDefaultFieldsFor verifies REQ-CLS-003: every request type the API exposes
// has a declared field set, so a caller that names no projection still gets a
// routable request. An unknown type returns nothing rather than a plausible
// guess -- the classifier turns that into an explicit failure.
func TestDefaultFieldsFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		requestType string
		want        []string
	}{
		// Status only: subState is omitempty decoration, and requiring it
		// would make every answer from a source that does not model it look
		// incomplete.
		{"resource_status", []string{FieldStatus}},
		{"resource_configuration", []string{FieldConfiguration}},
		{"resource_read", []string{
			FieldStatus, FieldSubState, FieldConfiguration, FieldOwner,
			FieldMetrics, FieldTopology, FieldLabels, FieldCustomerID,
		}},
		{"execution_status", []string{FieldExecutionStatus, FieldWorkflowSteps}},
		{"execution_history", []string{FieldExecutionHistory}},
		{"resource_details", []string{
			FieldStatus, FieldSubState, FieldConfiguration, FieldOwner, FieldMetrics,
			FieldTopology, FieldLatestExecution, FieldExecutionHistory,
		}},
		{"no_such_type", nil},
		{"", nil},
	}
	for _, tc := range cases {
		name := tc.requestType
		if name == "" {
			name = "empty request type"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, DefaultFieldsFor(tc.requestType), tc.want, "default fields for %q", tc.requestType)
		})
	}

	t.Run("every default field is modelled by the catalogue", func(t *testing.T) {
		t.Parallel()
		// The classifier validates its own defaults against the catalogue, so a
		// request type whose defaults are not all modelled would fail every
		// request of that type at runtime rather than at review time.
		c := NewFieldCatalog(config.Default().Precedence)
		for _, rt := range []string{
			"resource_status", "resource_configuration", "resource_read",
			"execution_status", "execution_history", "resource_details",
		} {
			for _, f := range DefaultFieldsFor(rt) {
				testutil.True(t, len(c.Suppliers(f)) > 0,
					"%s defaults to field %q, which no configured source supplies", rt, f)
			}
		}
	})

	t.Run("the request type is matched case-insensitively", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, DefaultFieldsFor("RESOURCE_STATUS"), []string{FieldStatus},
			"casing of the request type must not change the answer")
	})
}
