package mapper

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// ---------------------------------------------------------------------------
// Enum mappings
// ---------------------------------------------------------------------------

// TestMapExecutionStatus verifies REQ-MAP-002 and REQ-MAP-004: every EDS state
// token has an explicit canonical counterpart, matching is case-insensitive and
// whitespace-tolerant, and anything unrecognised becomes UNKNOWN rather than a
// plausible-looking neighbour.
func TestMapExecutionStatus(t *testing.T) {
	t.Parallel()

	table := map[string]domain.ExecutionStatus{
		"PENDING":     domain.ExecQueued,
		"QUEUED":      domain.ExecQueued,
		"IN_PROGRESS": domain.ExecRunning,
		"RUNNING":     domain.ExecRunning,
		"SUCCEEDED":   domain.ExecCompleted,
		"ERRORED":     domain.ExecFailed,
		"ABORTED":     domain.ExecCancelled,
		"EXPIRED":     domain.ExecTimedOut,
	}

	// The declared table and the mapping table must agree, so a token added to
	// one and not the other fails here.
	testutil.Equal(t, len(executionStatus), len(table), "every declared EDS token is covered")

	for token, want := range table {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, MapExecutionStatus(token), want, "%q", token)
			testutil.Equal(t, MapExecutionStatus(strings.ToLower(token)), want, "lower-cased %q", token)
			testutil.Equal(t, MapExecutionStatus("  "+token+"\t"), want, "padded %q", token)
			testutil.NotEqual(t, want, domain.ExecUnknown, "%q must not map to UNKNOWN", token)
		})
	}

	for _, unknown := range []string{"", "   ", "COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT", "in-progress", "surprise"} {
		t.Run("unknown/"+unknown, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, MapExecutionStatus(unknown), domain.ExecUnknown,
				"%q is not an EDS token and must map to UNKNOWN", unknown)
		})
	}
}

// TestMapResultingResourceStatus verifies REQ-MAP-002 and REQ-MAP-004: the
// EDS's opinion of the post-execution resource state maps explicitly into the
// canonical resource vocabulary.
func TestMapResultingResourceStatus(t *testing.T) {
	t.Parallel()

	table := map[string]domain.ResourceStatus{
		"LIVE":        domain.StatusActive,
		"ACTIVE":      domain.StatusActive,
		"PAUSED":      domain.StatusSuspended,
		"SUSPENDED":   domain.StatusSuspended,
		"IMPAIRED":    domain.StatusDegraded,
		"REMOVING":    domain.StatusTerminating,
		"REMOVED":     domain.StatusTerminated,
		"FAILED":      domain.StatusError,
		"PROVISIONED": domain.StatusActive,
		"BUILDING":    domain.StatusPending,
	}
	testutil.Equal(t, len(resultingResourceStatus), len(table), "every declared token is covered")

	for token, want := range table {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, MapResultingResourceStatus(token), want, "%q", token)
			testutil.Equal(t, MapResultingResourceStatus(strings.ToLower(token)), want, "lower-cased %q", token)
			testutil.Equal(t, MapResultingResourceStatus(" \n"+token+" "), want, "padded %q", token)
			testutil.NotEqual(t, want, domain.StatusUnknown, "%q must not map to UNKNOWN", token)
		})
	}

	for _, unknown := range []string{"", "  ", "DEGRADED", "TERMINATING", "PENDING", "who-knows"} {
		t.Run("unknown/"+unknown, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, MapResultingResourceStatus(unknown), domain.StatusUnknown,
				"%q must map to UNKNOWN", unknown)
		})
	}
}

// ---------------------------------------------------------------------------
// One()
// ---------------------------------------------------------------------------

// TestExecution_One_Rejects verifies REQ-MAP-008, REQ-MT-004 and REQ-EDGE-020:
// a record without identity, without a trustworthy tenant, or declaring an
// unsupported schema is refused with the right taxonomy code.
func TestExecution_One_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  *EDSExecution
		want errs.Code
	}{
		{name: "nil record", src: nil, want: errs.CodeUpstreamInvalidPayload},
		{
			name: "foreign tenant",
			src:  &EDSExecution{ExecutionID: "exec-1", TenantID: "tenant-b"},
			want: errs.CodeTenantMismatch,
		},
		{
			name: "missing execution id",
			src:  &EDSExecution{TenantID: "tenant-a", ResourceID: "res-1"},
			want: errs.CodeUpstreamInvalidPayload,
		},
		{
			name: "unknown schema version",
			src:  &EDSExecution{ExecutionID: "exec-1", TenantID: "tenant-a", SchemaVersion: "eds.v9"},
			want: errs.CodeSchemaVersionMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewExecution([]string{ExecutionSchemaVersion}, nil)
			got, err := m.One(tc.src, "tenant-a")
			testutil.ErrCode(t, err, tc.want, "%s must be refused", tc.name)
			testutil.True(t, got == nil, "no execution is returned on failure")
			e, ok := errs.As(err)
			testutil.True(t, ok, "the error carries the taxonomy type")
			testutil.Equal(t, e.Source, string(domain.SourceExecution), "the failing source is named")
		})
	}
}

// TestExecution_One_TenantResolution verifies REQ-MT-004: the tenant check only
// fires when there is something to compare, and the resolved tenant falls back
// to the one the BFF asked for.
func TestExecution_One_TenantResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		record     string
		requested  string
		wantErr    bool
		wantTenant string
	}{
		{name: "match", record: "tenant-a", requested: "tenant-a", wantTenant: "tenant-a"},
		{name: "mismatch", record: "tenant-b", requested: "tenant-a", wantErr: true},
		{name: "record omits tenant", record: "", requested: "tenant-a", wantTenant: "tenant-a"},
		{name: "caller omits tenant", record: "tenant-b", requested: "", wantTenant: "tenant-b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewExecution(nil, nil)
			got, err := m.One(&EDSExecution{ExecutionID: "exec-1", TenantID: tc.record}, tc.requested)
			if tc.wantErr {
				testutil.ErrCode(t, err, errs.CodeTenantMismatch, "a foreign record must be refused")
				return
			}
			testutil.NoError(t, err, "mapping")
			testutil.Equal(t, got.TenantID, tc.wantTenant, "resolved tenant")
		})
	}
}

// TestExecution_One_FieldMapping verifies REQ-MAP-005: every EDS field lands in
// its canonical home, including the renames (state -> status, wfSteps -> steps,
// outcomeCode -> outcome, failureCode -> error.code, auditTrail -> audit).
func TestExecution_One_FieldMapping(t *testing.T) {
	t.Parallel()

	started := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2024, 6, 1, 10, 5, 0, 0, time.UTC)
	updated := time.Date(2024, 6, 1, 10, 6, 0, 0, time.UTC)

	src := &EDSExecution{
		ExecutionID:            "exec-1",
		TenantID:               "tenant-a",
		ResourceID:             "res-1",
		CustomerID:             "cust-42",
		Operation:              "scale-out",
		State:                  "ERRORED",
		ResultingResourceState: "IMPAIRED",
		StartedAt:              started.Format(time.RFC3339),
		CompletedAt:            completed.Format(time.RFC3339),
		UpdatedAt:              updated.Format(time.RFC3339),
		WFSteps: []EDSStep{
			{
				StepID: "step-1", Label: "provision", Ordinal: 1, State: "SUCCEEDED",
				StartedAt: started.Format(time.RFC3339), CompletedAt: completed.Format(time.RFC3339),
				Attempt: 2,
			},
			{
				StepID: "step-2", Label: "verify", Ordinal: 2, State: "ERRORED",
				Failure: &EDSFailure{FailureCode: "TIMEOUT", Detail: "probe never answered", Transient: true, AtStep: "verify"},
			},
			// A step with no id cannot be addressed and is dropped.
			{Label: "ghost", Ordinal: 3, State: "PENDING"},
		},
		Actions: []EDSAction{
			{ActionID: "act-1", Kind: "api-call", TargetRef: "res-1", Result: "ok",
				ExecutedAt: completed.Format(time.RFC3339), ExecutedBy: "svc-orchestrator"},
			// A nameless action is dropped.
			{Kind: "api-call"},
		},
		Outcome: &EDSOutcome{
			OutcomeCode: "PARTIAL",
			Narrative:   "two of three replicas came up",
			Attributes:  map[string]string{"replicas": "2"},
		},
		Failure: &EDSFailure{FailureCode: "REPLICA_FAILED", Detail: "replica 3 did not start", Transient: false, AtStep: "verify"},
		Audit: []EDSAudit{
			{Timestamp: started.Format(time.RFC3339), Actor: "alice", Event: "STARTED", Detail: "manual"},
			{Timestamp: "", Actor: "system", Event: "RETRIED"},
		},
		SchemaVersion: ExecutionSchemaVersion,
	}

	m := NewExecution([]string{ExecutionSchemaVersion}, nil)
	got, err := m.One(src, "tenant-a")
	testutil.NoError(t, err, "mapping a complete record")

	testutil.Equal(t, got.ExecutionID, "exec-1", "executionId")
	testutil.Equal(t, got.TenantID, "tenant-a", "tenantId")
	testutil.Equal(t, got.ResourceID, "res-1", "resourceId")
	testutil.Equal(t, got.CustomerID, "cust-42", "customerId")
	testutil.Equal(t, got.Operation, "scale-out", "operation")
	testutil.Equal(t, got.Status, domain.ExecFailed, "state -> status (rename)")
	testutil.Equal(t, got.ResourceStatusAfter, domain.StatusDegraded,
		"resultingResourceState is a candidate resource status, never applied implicitly")
	testutil.Equal(t, got.SchemaVersion, ExecutionSchemaVersion, "schema version is carried")

	testutil.Equal(t, got.StartedAt, started, "startedAt")
	testutil.Equal(t, got.CompletedAt, completed, "completedAt")
	testutil.Equal(t, got.UpdatedAt, updated, "updatedAt")

	testutil.Equal(t, len(got.Steps), 2, "wfSteps -> steps, dropping the step with no id")
	testutil.Equal(t, got.Steps[0], domain.WorkflowStep{
		ID: "step-1", Name: "provision", Sequence: 1, Status: domain.ExecCompleted,
		StartedAt: started, CompletedAt: completed, Attempt: 2,
	}, "step 1: stepId->id, label->name, ordinal->sequence")
	testutil.Equal(t, got.Steps[1].Error, &domain.ExecutionError{
		Code: "TIMEOUT", Message: "probe never answered", Retryable: true, Step: "verify",
	}, "step failure -> step error (failureCode->code, detail->message, transient->retryable)")

	testutil.Equal(t, len(got.Actions), 1, "actions, dropping the one with no id")
	testutil.Equal(t, got.Actions[0], domain.Action{
		ID: "act-1", Type: "api-call", Target: "res-1", Outcome: "ok",
		PerformedAt: completed, PerformedBy: "svc-orchestrator",
	}, "kind->type, targetRef->target, result->outcome, executedBy->performedBy")

	testutil.Equal(t, got.Result, &domain.ExecutionResult{
		Outcome: "PARTIAL", Summary: "two of three replicas came up",
		Values: map[string]string{"replicas": "2"},
	}, "outcomeCode->outcome, narrative->summary, attributes->values")

	testutil.Equal(t, got.Error, &domain.ExecutionError{
		Code: "REPLICA_FAILED", Message: "replica 3 did not start", Retryable: false, Step: "verify",
	}, "failure -> error")

	testutil.Equal(t, len(got.Audit), 2, "auditTrail -> audit, keeping every entry")
	testutil.Equal(t, got.Audit[0], domain.AuditEntry{
		At: started, Actor: "alice", Action: "STARTED", Details: "manual",
	}, "event->action, detail->details")
	testutil.True(t, got.Audit[1].At.IsZero(), "an audit entry with no timestamp carries the zero time")
}

// TestExecution_One_OmitsEmptyStructures verifies REQ-MAP-006: absent optional
// structures stay absent; the mapper never fabricates an empty one.
func TestExecution_One_OmitsEmptyStructures(t *testing.T) {
	t.Parallel()

	m := NewExecution(nil, nil)
	got, err := m.One(&EDSExecution{
		ExecutionID: "exec-1",
		// An outcome with neither a code nor a narrative is not an outcome.
		Outcome: &EDSOutcome{Attributes: map[string]string{"x": "y"}},
		// A failure with neither a code nor a detail is not a failure.
		Failure: &EDSFailure{Transient: true},
		WFSteps: []EDSStep{},
		Actions: []EDSAction{},
		Audit:   []EDSAudit{},
	}, "tenant-a")
	testutil.NoError(t, err, "mapping a sparse record")

	testutil.True(t, got.Result == nil, "an outcome with no code or narrative yields no result")
	testutil.True(t, got.Error == nil, "a failure with no code or detail yields no error")
	testutil.True(t, got.Steps == nil, "an empty step list yields nil")
	testutil.True(t, got.Actions == nil, "an empty action list yields nil")
	testutil.True(t, got.Audit == nil, "an empty audit list yields nil")
	testutil.Equal(t, got.Status, domain.ExecUnknown, "an unset state maps to UNKNOWN")
	testutil.Equal(t, got.ResourceStatusAfter, domain.StatusUnknown, "an unset resulting state maps to UNKNOWN")
	testutil.True(t, got.StartedAt.IsZero(), "no timestamps are invented")
	testutil.True(t, got.UpdatedAt.IsZero(), "no timestamps are invented")
}

// TestExecution_SchemaVersion verifies REQ-EDGE-017: an unset schema version is
// tolerated, a known one is accepted, an unknown one is terminal and fires the
// mismatch callback.
func TestExecution_SchemaVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		accepted []string
		declared string
		wantErr  bool
		wantSeen []string
	}{
		{name: "unset accepted", accepted: []string{ExecutionSchemaVersion}, declared: ""},
		{name: "known accepted", accepted: []string{ExecutionSchemaVersion}, declared: ExecutionSchemaVersion},
		{name: "no gate accepts anything", accepted: nil, declared: "eds.v99"},
		{name: "unknown refused", accepted: []string{ExecutionSchemaVersion}, declared: "eds.v2", wantErr: true, wantSeen: []string{"eds.v2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen []string
			m := NewExecution(tc.accepted, func(got string) { seen = append(seen, got) })
			_, err := m.One(&EDSExecution{ExecutionID: "exec-1", SchemaVersion: tc.declared}, "tenant-a")
			if tc.wantErr {
				testutil.ErrCode(t, err, errs.CodeSchemaVersionMismatch, "unknown schema version is terminal")
			} else {
				testutil.NoError(t, err, "schema version %q should be accepted", tc.declared)
			}
			testutil.Equal(t, seen, tc.wantSeen, "mismatch callback invocations")
		})
	}
}

// ---------------------------------------------------------------------------
// Timestamps
// ---------------------------------------------------------------------------

// TestParseTime verifies REQ-MAP-010: RFC 3339 with or without fractional
// seconds parses and normalises to UTC, garbage yields the zero time, and the
// candidate list is tried in order.
func TestParseTime(t *testing.T) {
	t.Parallel()

	want := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		candidates []string
		want       time.Time
	}{
		{name: "rfc3339", candidates: []string{"2024-06-01T10:00:00Z"}, want: want},
		{name: "rfc3339 nano", candidates: []string{"2024-06-01T10:00:00.123456789Z"}, want: want.Add(123456789)},
		{name: "rfc3339 milliseconds", candidates: []string{"2024-06-01T10:00:00.250Z"}, want: want.Add(250 * time.Millisecond)},
		{name: "offset normalised to utc", candidates: []string{"2024-06-01T12:00:00+02:00"}, want: want},
		{name: "negative offset normalised to utc", candidates: []string{"2024-06-01T05:00:00-05:00"}, want: want},
		{name: "surrounding whitespace", candidates: []string{"  2024-06-01T10:00:00Z\n"}, want: want},
		{name: "no candidates", candidates: nil, want: time.Time{}},
		{name: "all empty", candidates: []string{"", "   ", ""}, want: time.Time{}},
		{name: "garbage", candidates: []string{"yesterday"}, want: time.Time{}},
		{name: "date only", candidates: []string{"2024-06-01"}, want: time.Time{}},
		{name: "unix seconds", candidates: []string{"1717236000"}, want: time.Time{}},
		{name: "skips empties", candidates: []string{"", "  ", "2024-06-01T10:00:00Z"}, want: want},
		{name: "first parseable wins", candidates: []string{"2024-06-01T10:00:00Z", "2020-01-01T00:00:00Z"}, want: want},
		// The chain accepts the first *parseable* candidate, so an unusable value
		// in an earlier field does not cost the timestamp entirely.
		{name: "unparseable candidate is skipped", candidates: []string{"soon", "2024-06-01T10:00:00Z"}, want: want},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseTime(tc.candidates...)
			testutil.Equal(t, got, tc.want, "parseTime(%v)", tc.candidates)
			if !tc.want.IsZero() {
				testutil.Equal(t, got.Location(), time.UTC, "parsed instants are normalised to UTC")
			}
		})
	}
}

// TestExecution_One_TimestampFallbackChain verifies REQ-MAP-010 and the EDS
// contract: updatedAt falls back through completedAt, executedAt and startedAt,
// because the source is inconsistent about which field it populates.
func TestExecution_One_TimestampFallbackChain(t *testing.T) {
	t.Parallel()

	var (
		startedAt   = "2024-06-01T10:00:00Z"
		executedAt  = "2024-06-01T10:01:00Z"
		completedAt = "2024-06-01T10:02:00Z"
		updatedAt   = "2024-06-01T10:03:00Z"
	)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", s, err)
		}
		return v.UTC()
	}

	cases := []struct {
		name        string
		src         EDSExecution
		wantStarted time.Time
		wantUpdated time.Time
	}{
		{
			name:        "every field present: updatedAt wins",
			src:         EDSExecution{StartedAt: startedAt, ExecutedAt: executedAt, CompletedAt: completedAt, UpdatedAt: updatedAt},
			wantStarted: at(startedAt),
			wantUpdated: at(updatedAt),
		},
		{
			name:        "no updatedAt: completedAt wins",
			src:         EDSExecution{StartedAt: startedAt, ExecutedAt: executedAt, CompletedAt: completedAt},
			wantStarted: at(startedAt),
			wantUpdated: at(completedAt),
		},
		{
			name:        "no updatedAt or completedAt: executedAt wins",
			src:         EDSExecution{StartedAt: startedAt, ExecutedAt: executedAt},
			wantStarted: at(startedAt),
			wantUpdated: at(executedAt),
		},
		{
			name:        "only startedAt: startedAt wins",
			src:         EDSExecution{StartedAt: startedAt},
			wantStarted: at(startedAt),
			wantUpdated: at(startedAt),
		},
		{
			name:        "no startedAt: executedAt stands in for it",
			src:         EDSExecution{ExecutedAt: executedAt},
			wantStarted: at(executedAt),
			wantUpdated: at(executedAt),
		},
		{
			name:        "nothing at all",
			src:         EDSExecution{},
			wantStarted: time.Time{},
			wantUpdated: time.Time{},
		},
	}

	m := NewExecution(nil, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := tc.src
			src.ExecutionID = "exec-1"
			got, err := m.One(&src, "tenant-a")
			testutil.NoError(t, err, "mapping")
			testutil.Equal(t, got.StartedAt, tc.wantStarted, "startedAt")
			testutil.Equal(t, got.UpdatedAt, tc.wantUpdated, "updatedAt")
		})
	}
}

// ---------------------------------------------------------------------------
// Page()
// ---------------------------------------------------------------------------

// TestExecution_Page_DropsInvalidRecords verifies REQ-EDGE-020: one malformed
// history row must not blank an otherwise useful list. The row is dropped and
// the caller is told how many were lost so it can warn.
func TestExecution_Page_DropsInvalidRecords(t *testing.T) {
	t.Parallel()

	m := NewExecution([]string{ExecutionSchemaVersion}, nil)
	page := &EDSExecutionPage{
		TotalCount: 5,
		NextCursor: "cursor-2",
		Items: []EDSExecution{
			{ExecutionID: "exec-1", TenantID: "tenant-a", ResourceID: "res-1", UpdatedAt: "2024-06-01T10:00:00Z"},
			// No execution id: unaddressable, dropped.
			{TenantID: "tenant-a", ResourceID: "res-1"},
			{ExecutionID: "exec-2", TenantID: "tenant-a", ResourceID: "res-1", UpdatedAt: "2024-06-01T09:00:00Z"},
			// Unsupported schema: dropped, not fatal for the page.
			{ExecutionID: "exec-3", TenantID: "tenant-a", SchemaVersion: "eds.v9"},
		},
	}

	got, dropped, err := m.Page(page, "tenant-a")
	testutil.NoError(t, err, "a page with droppable rows still maps")
	testutil.Equal(t, dropped, 2, "both unusable rows are counted")
	testutil.Equal(t, len(got.Items), 2, "the usable rows survive")
	testutil.Equal(t, got.Items[0].ExecutionID, "exec-1", "first surviving item")
	testutil.Equal(t, got.Items[1].ExecutionID, "exec-2", "second surviving item")
	testutil.Equal(t, got.Total, 5, "the source's total count is carried verbatim")
	testutil.Equal(t, got.NextCursor, "cursor-2", "the cursor is carried verbatim")
	testutil.Equal(t, got.ResourceID, "res-1", "the resource id is taken from the surviving items")
}

// TestExecution_Page_TenantMismatchFailsWholePage verifies REQ-MT-004 and
// REQ-EDGE-020: a foreign record inside a page is not a droppable row. It means
// the source is returning another tenant's data, so the whole response is
// untrustworthy and none of it is served.
func TestExecution_Page_TenantMismatchFailsWholePage(t *testing.T) {
	t.Parallel()

	m := NewExecution(nil, nil)
	page := &EDSExecutionPage{
		TotalCount: 3,
		Items: []EDSExecution{
			{ExecutionID: "exec-1", TenantID: "tenant-a", ResourceID: "res-1"},
			{ExecutionID: "exec-2", TenantID: "tenant-b", ResourceID: "res-9"},
			{ExecutionID: "exec-3", TenantID: "tenant-a", ResourceID: "res-1"},
		},
	}

	got, dropped, err := m.Page(page, "tenant-a")
	testutil.ErrCode(t, err, errs.CodeTenantMismatch, "a cross-tenant row poisons the whole page")
	testutil.True(t, got == nil, "no partial list is returned")
	testutil.Equal(t, dropped, 0, "a poisoned page reports no drop count")
}

// TestExecution_Page_Edges verifies REQ-EDGE-019/020: an absent page is invalid,
// while an empty page is a valid empty list rather than an error.
func TestExecution_Page_Edges(t *testing.T) {
	t.Parallel()

	m := NewExecution(nil, nil)

	t.Run("nil page", func(t *testing.T) {
		t.Parallel()
		got, dropped, err := m.Page(nil, "tenant-a")
		testutil.ErrCode(t, err, errs.CodeUpstreamInvalidPayload, "an absent page is an invalid response")
		testutil.True(t, got == nil, "no list is returned")
		testutil.Equal(t, dropped, 0, "no drop count")
	})

	t.Run("empty page", func(t *testing.T) {
		t.Parallel()
		got, dropped, err := m.Page(&EDSExecutionPage{}, "tenant-a")
		testutil.NoError(t, err, "an empty page is a valid empty list")
		testutil.Equal(t, dropped, 0, "nothing was dropped")
		testutil.Equal(t, len(got.Items), 0, "no items")
		testutil.True(t, got.Items != nil, "the item slice is allocated so it serialises as [] rather than null")
		testutil.Equal(t, got.ResourceID, "", "no resource id can be inferred from an empty page")
	})

	t.Run("every row invalid", func(t *testing.T) {
		t.Parallel()
		got, dropped, err := m.Page(&EDSExecutionPage{
			TotalCount: 2,
			Items:      []EDSExecution{{ResourceID: "res-1"}, {ResourceID: "res-1"}},
		}, "tenant-a")
		testutil.NoError(t, err, "a page of unusable rows is still a page")
		testutil.Equal(t, dropped, 2, "every row is counted as dropped")
		testutil.Equal(t, len(got.Items), 0, "nothing survives")
	})
}

// TestExecution_Page_SortsNewestFirst verifies REQ-AGG-005: ordering is
// explicit and does not depend on the order the source happened to return, and
// it uses the most specific timestamp each record carries.
func TestExecution_Page_SortsNewestFirst(t *testing.T) {
	t.Parallel()

	m := NewExecution(nil, nil)

	t.Run("shuffled updatedAt", func(t *testing.T) {
		t.Parallel()
		got, dropped, err := m.Page(&EDSExecutionPage{Items: []EDSExecution{
			{ExecutionID: "middle", ResourceID: "res-1", UpdatedAt: "2024-06-01T11:00:00Z"},
			{ExecutionID: "oldest", ResourceID: "res-1", UpdatedAt: "2024-06-01T09:00:00Z"},
			{ExecutionID: "newest", ResourceID: "res-1", UpdatedAt: "2024-06-01T13:00:00Z"},
		}}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, dropped, 0, "nothing dropped")
		testutil.Equal(t, ids(got.Items), []string{"newest", "middle", "oldest"}, "newest first")
	})

	t.Run("already ordered input is left alone", func(t *testing.T) {
		t.Parallel()
		got, _, err := m.Page(&EDSExecutionPage{Items: []EDSExecution{
			{ExecutionID: "newest", ResourceID: "res-1", UpdatedAt: "2024-06-01T13:00:00Z"},
			{ExecutionID: "middle", ResourceID: "res-1", UpdatedAt: "2024-06-01T11:00:00Z"},
			{ExecutionID: "oldest", ResourceID: "res-1", UpdatedAt: "2024-06-01T09:00:00Z"},
		}}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, ids(got.Items), []string{"newest", "middle", "oldest"}, "order preserved")
	})

	t.Run("mixed timestamp sources", func(t *testing.T) {
		t.Parallel()
		// Each record carries a different "most specific" timestamp; ordering
		// must still be by the effective instant, not by which field was set.
		got, _, err := m.Page(&EDSExecutionPage{Items: []EDSExecution{
			{ExecutionID: "started-only", ResourceID: "res-1", StartedAt: "2024-06-01T08:00:00Z"},
			{ExecutionID: "updated", ResourceID: "res-1", UpdatedAt: "2024-06-01T12:00:00Z"},
			{ExecutionID: "completed-only", ResourceID: "res-1", CompletedAt: "2024-06-01T10:00:00Z"},
		}}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, ids(got.Items), []string{"updated", "completed-only", "started-only"}, "newest first")
	})

	t.Run("records without timestamps sort last but keep their relative order", func(t *testing.T) {
		t.Parallel()
		got, _, err := m.Page(&EDSExecutionPage{Items: []EDSExecution{
			{ExecutionID: "undated-a", ResourceID: "res-1"},
			{ExecutionID: "dated", ResourceID: "res-1", UpdatedAt: "2024-06-01T09:00:00Z"},
			{ExecutionID: "undated-b", ResourceID: "res-1"},
		}}, "tenant-a")
		testutil.NoError(t, err, "mapping")
		testutil.Equal(t, ids(got.Items), []string{"dated", "undated-a", "undated-b"},
			"the sort is stable, so equal keys keep source order")
	})
}

func ids(items []domain.Execution) []string {
	out := make([]string, 0, len(items))
	for _, e := range items {
		out = append(out, e.ExecutionID)
	}
	return out
}
