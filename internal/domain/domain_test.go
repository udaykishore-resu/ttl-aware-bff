package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

var base = time.Date(2026, 2, 17, 10, 30, 0, 0, time.UTC)

// TestParseSourceKind verifies REQ-MAP-001: configuration and wire strings are
// mapped to the canonical source kinds explicitly. An unrecognised token is
// refused rather than mapped to a default, because silently reading "operatinal"
// as the operational source is how a precedence table stops meaning what it says.
func TestParseSourceKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in    string
		want  domain.SourceKind
		valid bool
	}{
		{"operational", domain.SourceOperational, true},
		{"OPERATIONAL", domain.SourceOperational, true},
		{"  Operational  ", domain.SourceOperational, true},
		{"ods", domain.SourceOperational, true},
		{"execution", domain.SourceExecution, true},
		{"eds", domain.SourceExecution, true},
		{"cache", domain.SourceCache, true},
		{"none", domain.SourceNone, true},
		{"", domain.SourceNone, true},
		{"operatinal", domain.SourceNone, false},
		{"both", domain.SourceNone, false},
		{"unknown", domain.SourceNone, false},
	}
	for _, tc := range cases {
		t.Run("token_"+tc.in, func(t *testing.T) {
			t.Parallel()
			got, ok := domain.ParseSourceKind(tc.in)
			testutil.Equal(t, ok, tc.valid, "validity of %q", tc.in)
			testutil.Equal(t, got, tc.want, "kind for %q", tc.in)
		})
	}

	t.Run("every canonical kind round-trips through its own name", func(t *testing.T) {
		t.Parallel()
		for _, k := range []domain.SourceKind{domain.SourceOperational, domain.SourceExecution, domain.SourceCache, domain.SourceNone} {
			got, ok := domain.ParseSourceKind(string(k))
			testutil.True(t, ok, "%q must parse", k)
			testutil.Equal(t, got, k, "round trip of %q", k)
			testutil.True(t, k.Valid(), "%q is a defined kind", k)
		}
		testutil.False(t, domain.SourceKind("BOTH").Valid(), "an undefined kind is not valid")
		testutil.False(t, domain.SourceKind("").Valid(), "nor is the zero value")
	})
}

// TestSourceKind_IsNone verifies the invariant this method exists for: the zero
// value of SourceKind is "" while the explicit no-source constant is "NONE", so
// a bare `k == SourceNone` comparison silently misses an unset field. Both
// spellings must report the same thing, and neither may be confused with a real
// source.
func TestSourceKind_IsNone(t *testing.T) {
	t.Parallel()

	cases := map[domain.SourceKind]bool{
		domain.SourceNone:        true,
		domain.SourceKind(""):    true,
		domain.SourceOperational: false,
		domain.SourceExecution:   false,
		domain.SourceCache:       false,
	}
	for k, want := range cases {
		name := string(k)
		if name == "" {
			name = "zero value"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, k.IsNone(), want, "IsNone for %q", k)
		})
	}

	t.Run("an unset provenance entry reads as no source", func(t *testing.T) {
		t.Parallel()
		// This is the shape the bug took: a map lookup that misses returns the
		// zero value, not SourceNone.
		provenance := map[string]domain.SourceKind{}
		testutil.True(t, provenance["status"].IsNone(),
			"a field nothing supplied must report no source, however the absence is spelled")
	})
}

// TestExecutionStatus_TerminalAndInProgress verifies REQ-PREC-003's precondition:
// "is a workflow currently mutating this resource?" is answered by this pair,
// and the precedence override hangs off it. Every member of the vocabulary is
// asserted so a new status cannot be added without deciding which it is.
func TestExecutionStatus_TerminalAndInProgress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status     domain.ExecutionStatus
		terminal   bool
		inProgress bool
	}{
		{domain.ExecUnknown, false, false},
		{domain.ExecQueued, false, true},
		{domain.ExecRunning, false, true},
		{domain.ExecCompleted, true, false},
		{domain.ExecFailed, true, false},
		{domain.ExecCancelled, true, false},
		{domain.ExecTimedOut, true, false},
		{domain.ExecutionStatus(""), false, false},
	}
	for _, tc := range cases {
		name := string(tc.status)
		if name == "" {
			name = "zero value"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, tc.status.Terminal(), tc.terminal, "Terminal for %q", tc.status)
			testutil.Equal(t, tc.status.InProgress(), tc.inProgress, "InProgress for %q", tc.status)
			testutil.False(t, tc.terminal && tc.inProgress,
				"%q cannot be both finished and still running", tc.status)
		})
	}
}

// TestSortExecutionsByRecency verifies REQ-AGG-003: history order is decided
// here rather than inherited from whichever source produced the page, because
// the two sources make no matching ordering guarantee. The key is the most
// specific timestamp an execution has, and equal keys keep their input order so
// a page never reshuffles between identical requests.
func TestSortExecutionsByRecency(t *testing.T) {
	t.Parallel()

	t.Run("updatedAt is preferred", func(t *testing.T) {
		t.Parallel()
		execs := []domain.Execution{
			{ExecutionID: "old", UpdatedAt: base.Add(-time.Hour)},
			{ExecutionID: "new", UpdatedAt: base},
			{ExecutionID: "middle", UpdatedAt: base.Add(-30 * time.Minute)},
		}
		domain.SortExecutionsByRecency(execs)
		testutil.Equal(t, ids(execs), []string{"new", "middle", "old"}, "newest first")
	})

	t.Run("completedAt is the fallback", func(t *testing.T) {
		t.Parallel()
		// An execution with no updatedAt must still be placed by when it
		// finished, not treated as infinitely old.
		execs := []domain.Execution{
			{ExecutionID: "completed-recently", CompletedAt: base, StartedAt: base.Add(-time.Hour)},
			{ExecutionID: "updated-earlier", UpdatedAt: base.Add(-10 * time.Minute)},
		}
		domain.SortExecutionsByRecency(execs)
		testutil.Equal(t, ids(execs), []string{"completed-recently", "updated-earlier"}, "order")
	})

	t.Run("startedAt is the last resort", func(t *testing.T) {
		t.Parallel()
		// A still-running execution has neither of the other two timestamps.
		execs := []domain.Execution{
			{ExecutionID: "started-earlier", StartedAt: base.Add(-time.Hour)},
			{ExecutionID: "started-later", StartedAt: base},
		}
		domain.SortExecutionsByRecency(execs)
		testutil.Equal(t, ids(execs), []string{"started-later", "started-earlier"}, "order")
	})

	t.Run("the most specific timestamp wins over an older, more specific one", func(t *testing.T) {
		t.Parallel()
		execs := []domain.Execution{
			{ExecutionID: "started-late-updated-early", StartedAt: base, UpdatedAt: base.Add(-time.Hour)},
			{ExecutionID: "updated-now", UpdatedAt: base.Add(-time.Minute)},
		}
		domain.SortExecutionsByRecency(execs)
		testutil.Equal(t, ids(execs), []string{"updated-now", "started-late-updated-early"},
			"updatedAt is the key whenever it is present, even if another field is newer")
	})

	t.Run("the sort is stable", func(t *testing.T) {
		t.Parallel()
		execs := []domain.Execution{
			{ExecutionID: "a", UpdatedAt: base},
			{ExecutionID: "b", UpdatedAt: base},
			{ExecutionID: "c", UpdatedAt: base},
			{ExecutionID: "d", UpdatedAt: base.Add(time.Second)},
		}
		domain.SortExecutionsByRecency(execs)
		testutil.Equal(t, ids(execs), []string{"d", "a", "b", "c"},
			"executions sharing a timestamp keep their input order, so paging is repeatable")
	})

	t.Run("degenerate inputs", func(t *testing.T) {
		t.Parallel()
		domain.SortExecutionsByRecency(nil)
		one := []domain.Execution{{ExecutionID: "only"}}
		domain.SortExecutionsByRecency(one)
		testutil.Equal(t, ids(one), []string{"only"}, "a single execution is already sorted")
	})
}

func ids(execs []domain.Execution) []string {
	out := make([]string, 0, len(execs))
	for _, e := range execs {
		out = append(out, e.ExecutionID)
	}
	return out
}

// TestResource_IsZero verifies REQ-EDGE-019: "the source answered with nothing"
// and "we never asked" must be distinguishable, so emptiness is a property of
// the record's data rather than of the pointer.
func TestResource_IsZero(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  *domain.Resource
		want bool
	}{
		{"nil", nil, true},
		{"zero value", &domain.Resource{}, true},
		{"identity alone is not data", &domain.Resource{TenantID: "acme"}, true},
		{"a resource id makes it non-empty", &domain.Resource{ResourceID: "R1"}, false},
		{"a status makes it non-empty", &domain.Resource{Status: domain.StatusActive}, false},
		{"configuration makes it non-empty", &domain.Resource{Configuration: map[string]string{"tier": "gold"}}, false},
		{"metrics make it non-empty", &domain.Resource{Metrics: []domain.Metric{{Name: "cpu"}}}, false},
		{"an empty configuration map is still empty", &domain.Resource{Configuration: map[string]string{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, tc.res.IsZero(), tc.want, "IsZero")
		})
	}
}

// TestResource_Completeness verifies REQ-EDGE-006 and REQ-EDGE-007: a source
// can answer successfully and still return half a record, and the aggregator
// needs a number for that rather than a boolean. A status decoded to UNKNOWN is
// not a populated status, which is the same rule precedence applies.
func TestResource_Completeness(t *testing.T) {
	t.Parallel()

	full := &domain.Resource{
		ResourceID:    "R1",
		CustomerID:    "C-1",
		Type:          "database",
		Status:        domain.StatusActive,
		Owner:         &domain.Owner{ID: "team-1"},
		Configuration: map[string]string{"tier": "gold"},
		Metrics:       []domain.Metric{{Name: "cpu"}},
		Topology:      &domain.Topology{Region: "eu-west-1"},
	}
	testutil.Equal(t, full.Completeness(), 1.0, "every core field is populated")

	cases := []struct {
		name string
		res  *domain.Resource
		want float64
	}{
		{"nil", nil, 0},
		{"zero value", &domain.Resource{}, 0},
		{"two of eight", &domain.Resource{ResourceID: "R1", Status: domain.StatusActive}, 0.25},
		{"an UNKNOWN status does not count", &domain.Resource{ResourceID: "R1", Status: domain.StatusUnknown}, 0.125},
		{"an owner without an id does not count", &domain.Resource{ResourceID: "R1", Owner: &domain.Owner{}}, 0.125},
		{"a topology without a region does not count", &domain.Resource{ResourceID: "R1", Topology: &domain.Topology{}}, 0.125},
		{"empty collections do not count", &domain.Resource{
			ResourceID: "R1", Configuration: map[string]string{}, Metrics: []domain.Metric{},
		}, 0.125},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, tc.res.Completeness(), tc.want, "completeness")
		})
	}
}

// TestResponseMeta_AddWarning verifies that a warning is identified by what
// went wrong and where, so the same condition observed twice in one request is
// reported once -- while the same condition from two different sources stays
// two warnings, because they are two different facts.
func TestResponseMeta_AddWarning(t *testing.T) {
	t.Parallel()

	var m domain.ResponseMeta
	m.AddWarning(domain.Warning{Code: domain.WarnSourceTimeout, Message: "first wording", Source: domain.SourceExecution})
	m.AddWarning(domain.Warning{Code: domain.WarnSourceTimeout, Message: "second wording", Source: domain.SourceExecution})

	testutil.Equal(t, len(m.Warnings), 1, "the duplicate was dropped, got %v", m.Warnings)
	testutil.Equal(t, m.Warnings[0].Message, "first wording",
		"the first wording is kept: a later duplicate does not rewrite what was already reported")

	m.AddWarning(domain.Warning{Code: domain.WarnSourceTimeout, Message: "other source", Source: domain.SourceOperational})
	testutil.Equal(t, len(m.Warnings), 2, "the same code from another source is a distinct warning")

	m.AddWarning(domain.Warning{Code: domain.WarnStaleData, Message: "stale", Source: domain.SourceExecution})
	testutil.Equal(t, len(m.Warnings), 3, "as is another code from the same source")

	m.AddWarning(domain.Warning{Code: domain.WarnPartialData, Message: "no source attributed"})
	m.AddWarning(domain.Warning{Code: domain.WarnPartialData, Message: "again"})
	testutil.Equal(t, len(m.Warnings), 4, "a source-less warning de-duplicates on its code alone")
}

// TestResponseMeta_AddSource verifies that the response's source list is a set
// of the sources that actually contributed: no duplicates, and no entry for the
// absence of a source.
func TestResponseMeta_AddSource(t *testing.T) {
	t.Parallel()

	var m domain.ResponseMeta
	m.AddSource(domain.SourceOperational)
	m.AddSource(domain.SourceOperational)
	m.AddSource(domain.SourceExecution)
	m.AddSource(domain.SourceNone)
	m.AddSource("")
	m.AddSource(domain.SourceCache)

	testutil.Equal(t, m.Sources, []domain.SourceKind{domain.SourceOperational, domain.SourceExecution, domain.SourceCache},
		"contributing sources are listed once each, in the order they contributed")
}

// TestResponseMeta_SetProvenance verifies REQ-PREC-001's observable half:
// provenance is per field, is created on first use, and a later decision for
// the same field replaces the earlier one rather than accumulating.
func TestResponseMeta_SetProvenance(t *testing.T) {
	t.Parallel()

	var m domain.ResponseMeta
	testutil.True(t, m.Provenance == nil, "no provenance is recorded until a field is attributed")

	m.SetProvenance("status", domain.SourceOperational)
	m.SetProvenance("executionHistory", domain.SourceExecution)
	testutil.Equal(t, m.Provenance, map[string]domain.SourceKind{
		"status":           domain.SourceOperational,
		"executionHistory": domain.SourceExecution,
	}, "each field names the source that supplied it")

	m.SetProvenance("status", domain.SourceExecution)
	testutil.Equal(t, m.Provenance["status"], domain.SourceExecution, "a re-resolved field is overwritten")
	testutil.Equal(t, len(m.Provenance), 2, "and does not add an entry")
}

// TestFreshness_JSONRoundTrip verifies REQ-CACHE-003: age and TTL are held as
// time.Duration, which encoding/json would otherwise render as an opaque
// nanosecond count. They are emitted as seconds and read back as durations, so
// an entry written by one instance is understood by another -- and so the UI's
// "last updated N seconds ago" badge has a documented field to read.
func TestFreshness_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := domain.Freshness{
		State:         domain.FreshnessStale,
		Age:           2500 * time.Millisecond,
		TTL:           30 * time.Second,
		ObservedAt:    base.Add(-2500 * time.Millisecond),
		EvaluatedAt:   base,
		Source:        domain.SourceOperational,
		SkewCorrected: true,
		Version:       42,
	}

	encoded, err := json.Marshal(original)
	testutil.NoError(t, err, "marshal")
	testutil.True(t, strings.Contains(string(encoded), `"ageSeconds":2.5`),
		"the age is emitted in seconds, got %s", encoded)
	testutil.True(t, strings.Contains(string(encoded), `"ttlSeconds":30`),
		"as is the TTL, got %s", encoded)

	var decoded domain.Freshness
	testutil.NoError(t, json.Unmarshal(encoded, &decoded), "unmarshal")

	testutil.Equal(t, decoded.Age, original.Age, "the age survives the round trip")
	testutil.Equal(t, decoded.TTL, original.TTL, "as does the TTL")
	testutil.Equal(t, decoded.State, original.State, "and the verdict")
	testutil.True(t, decoded.ObservedAt.Equal(original.ObservedAt), "and the observation instant")
	testutil.True(t, decoded.EvaluatedAt.Equal(original.EvaluatedAt), "and the evaluation instant")
	testutil.Equal(t, decoded.Source, original.Source, "and the source")
	testutil.True(t, decoded.SkewCorrected, "and the skew flag")
	testutil.Equal(t, decoded.Version, original.Version, "and the record version")

	t.Run("sub-second ages survive", func(t *testing.T) {
		t.Parallel()
		// A fast operational source is the normal case, so the representation
		// has to hold ages far below one second without rounding them to zero.
		f := domain.Freshness{State: domain.FreshnessFresh, Age: 125 * time.Millisecond, TTL: 10 * time.Second}
		b, err := json.Marshal(f)
		testutil.NoError(t, err, "marshal")
		var got domain.Freshness
		testutil.NoError(t, json.Unmarshal(b, &got), "unmarshal")
		testutil.Equal(t, got.Age, 125*time.Millisecond, "a sub-second age is preserved")
	})

	t.Run("a zero freshness round-trips as zero", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(domain.Freshness{})
		testutil.NoError(t, err, "marshal")
		var got domain.Freshness
		testutil.NoError(t, json.Unmarshal(b, &got), "unmarshal")
		testutil.Equal(t, got.Age, time.Duration(0), "no age")
		testutil.Equal(t, got.TTL, time.Duration(0), "no TTL")
		testutil.True(t, got.ObservedAt.IsZero(), "no observation instant")
	})

	t.Run("IsFresh reads the verdict rather than recomputing it", func(t *testing.T) {
		t.Parallel()
		testutil.True(t, domain.Freshness{State: domain.FreshnessFresh}.IsFresh(), "fresh")
		testutil.False(t, domain.Freshness{State: domain.FreshnessStale, Age: 0}.IsFresh(),
			"a zero age does not override a stale verdict")
		testutil.False(t, domain.Freshness{State: domain.FreshnessUnknown}.IsFresh(), "unknown is not fresh")
		testutil.Equal(t, original.AgeSeconds(), 2.5, "the age accessor agrees with the wire format")
		testutil.Equal(t, original.TTLSeconds(), 30.0, "as does the TTL accessor")
	})
}
