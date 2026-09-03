package policy

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
)

var base = time.Date(2026, 4, 12, 8, 0, 0, 0, time.UTC)

// conflictLog captures the policy's conflict callback. It exists so a test can
// assert the *absence* of a warning: two sources agreeing must not produce one,
// or every /details response would carry noise the UI has to filter.
type conflictLog struct{ calls []string }

func (c *conflictLog) record(field string, winner domain.SourceKind) {
	c.calls = append(c.calls, field+"="+string(winner))
}

// newPolicy builds a policy over an explicit precedence table, so each test
// states the configuration it depends on instead of inheriting it.
func newPolicy(fields map[string][]string, overrides []string) (*SourcePrecedencePolicy, *conflictLog) {
	log := &conflictLog{}
	p := NewPrecedence(config.PrecedenceConfig{
		Fields:                        fields,
		ExecutionOverridesWhenRunning: overrides,
		ConflictWarning:               true,
	}, log.record)
	return p, log
}

// ops builds a present operational candidate.
func ops(v any) Candidate {
	return Candidate{Source: domain.SourceOperational, Present: true, ObservedAt: base, Value: v}
}

// exec builds a present execution candidate.
func exec(v any) Candidate {
	return Candidate{Source: domain.SourceExecution, Present: true, ObservedAt: base, Value: v}
}

// stale marks a candidate as past its freshness TTL.
func stale(c Candidate) Candidate {
	c.Stale = true
	return c
}

// TestResolve_NoCandidateHasNoWinner verifies REQ-PREC-005 at its limit: with
// nothing offered, the policy names no source rather than inventing one. The
// merger relies on this to leave the field untouched.
func TestResolve_NoCandidateHasNoWinner(t *testing.T) {
	t.Parallel()

	p, log := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

	cases := map[string][]Candidate{
		"no candidates at all": nil,
		"every candidate absent": {
			{Source: domain.SourceOperational},
			{Source: domain.SourceExecution},
		},
	}
	for name, candidates := range cases {
		t.Run(name, func(t *testing.T) {
			d := p.Resolve(FieldStatus, candidates, Context{})
			testutil.True(t, d.Winner.IsNone(), "no source may be named, got %q", d.Winner)
			testutil.Equal(t, d.Rule, RulePrecedenceNoCandidate, "rule id")
			testutil.True(t, d.Value == nil, "and no value is produced")
			testutil.False(t, d.Conflict, "one absent source cannot conflict with another")
		})
	}
	testutil.Equal(t, len(log.calls), 0, "nothing was resolved, so nothing may be counted as a conflict")
}

// TestResolve_SingleCandidateWins verifies REQ-PREC-001: when only one source
// answered there is no precedence question to ask, and the emitted rule says so
// rather than claiming configuration decided it.
func TestResolve_SingleCandidateWins(t *testing.T) {
	t.Parallel()

	// The configured order names the operational source first; the point of the
	// test is that this is irrelevant when only the execution source answered.
	p, log := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

	d := p.Resolve(FieldStatus, []Candidate{
		{Source: domain.SourceOperational},
		exec(domain.StatusTerminating),
	}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceExecution, "winner")
	testutil.Equal(t, d.Rule, RulePrecedenceOnlyCandidate, "rule id")
	testutil.Equal(t, d.Value, any(domain.StatusTerminating), "value")
	testutil.False(t, d.Conflict, "a single candidate cannot conflict with anything")
	testutil.Equal(t, len(d.Rejected), 0, "and there is nobody to reject")
	testutil.Equal(t, len(log.calls), 0, "no conflict was reported")
}

// TestResolve_AbsentCandidateNeverWins verifies REQ-PREC-005: `Present: false`
// means the source did not answer. Carrying a value is not enough -- a source
// that failed must never displace one that answered, whatever its rank.
func TestResolve_AbsentCandidateNeverWins(t *testing.T) {
	t.Parallel()

	// The absent source is the *higher* ranked one, so a bug that ignored
	// Present would show up here as an execution win.
	p, _ := newPolicy(map[string][]string{FieldStatus: {"execution", "operational"}}, nil)

	d := p.Resolve(FieldStatus, []Candidate{
		{Source: domain.SourceExecution, Present: false, Value: domain.StatusTerminated},
		ops(domain.StatusActive),
	}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceOperational,
		"the source that actually answered wins, even though it ranks second")
	testutil.Equal(t, d.Value, any(domain.StatusActive), "and its value is the one carried forward")
	testutil.Equal(t, d.Rule, RulePrecedenceOnlyCandidate,
		"with the absent candidate discarded there is only one candidate left")
}

// TestResolve_ConfiguredOrderDecides verifies REQ-PREC-001 and REQ-PREC-002:
// with two present candidates the answer comes from the configured per-field
// order and from nothing else. Reversing the configuration reverses the winner,
// which is the whole point of keeping precedence in data.
func TestResolve_ConfiguredOrderDecides(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		order []string
		want  domain.SourceKind
		value any
	}{
		{"operational first", []string{"operational", "execution"}, domain.SourceOperational, domain.StatusActive},
		{"execution first", []string{"execution", "operational"}, domain.SourceExecution, domain.StatusTerminated},
		// An unconfigured field still has to be deterministic: the aggregator
		// always assembles candidates in source order, so the first present one
		// wins and the same request always produces the same answer.
		{"no configured order falls back to candidate order", nil, domain.SourceOperational, domain.StatusActive},
		// A configured order naming a source that did not answer must not leave
		// the field unresolved.
		{"order naming only an absent source", []string{"cache"}, domain.SourceOperational, domain.StatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, log := newPolicy(map[string][]string{FieldStatus: tc.order}, nil)

			d := p.Resolve(FieldStatus, []Candidate{
				ops(domain.StatusActive),
				exec(domain.StatusTerminated),
			}, Context{})

			testutil.Equal(t, d.Winner, tc.want, "winner")
			testutil.Equal(t, d.Value, tc.value, "the winner's value travels with the decision")
			testutil.Equal(t, d.Rule, RulePrecedenceConfiguredOrder, "rule id")
			testutil.True(t, d.Conflict, "the two sources disagreed")
			testutil.Equal(t, len(log.calls), 1, "and the disagreement was counted exactly once")
		})
	}
}

// TestResolve_FreshBeatsStale verifies REQ-PREC-001 as applied to degraded
// data: within the configured order, a candidate past its TTL yields to one
// that is not, and the emitted rule id changes so provenance shows why the
// declared order was not followed.
func TestResolve_FreshBeatsStale(t *testing.T) {
	t.Parallel()

	p, _ := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

	d := p.Resolve(FieldStatus, []Candidate{
		stale(ops(domain.StatusActive)),
		exec(domain.StatusTerminated),
	}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceExecution,
		"the top-ranked source is past its TTL, so the fresh one answers")
	testutil.Equal(t, d.Rule, RulePrecedenceFreshBeatsStale,
		"the rule id must record that staleness, not configuration, moved the winner")
	testutil.Equal(t, d.Rejected, []domain.SourceKind{domain.SourceOperational}, "the loser is named")
}

// TestResolve_AllStaleStillUsesConfiguredOrder verifies REQ-PREC-001: when
// every candidate is past its TTL there is nothing to prefer, so the declared
// order decides as usual. The caller marks the response degraded; the policy
// does not silently change its mind about which source is authoritative.
func TestResolve_AllStaleStillUsesConfiguredOrder(t *testing.T) {
	t.Parallel()

	p, _ := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

	d := p.Resolve(FieldStatus, []Candidate{
		stale(ops(domain.StatusActive)),
		stale(exec(domain.StatusTerminated)),
	}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceOperational, "the declared order still decides")
	testutil.Equal(t, d.Rule, RulePrecedenceConfiguredOrder,
		"and the rule id is the ordinary one: no candidate was preferred for being fresh")
	testutil.Equal(t, d.Value, any(domain.StatusActive), "value")
}

// TestResolve_ExecutionOverridesWhenRunning verifies REQ-PREC-003: mid-flight,
// the execution source is authoritative about what is happening right now --
// but only for the fields configuration nominated, and only while an execution
// really is in progress. Both halves of that conjunction are tested, because
// either one alone would be a silent, hard-to-diagnose precedence inversion.
func TestResolve_ExecutionOverridesWhenRunning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		field      string
		inProgress bool
		wantWinner domain.SourceKind
		wantRule   string
	}{
		{"nominated field while running", FieldStatus, true, domain.SourceExecution, RulePrecedenceExecutionRunning},
		{"nominated field while idle", FieldStatus, false, domain.SourceOperational, RulePrecedenceConfiguredOrder},
		{"un-nominated field while running", FieldCustomerID, true, domain.SourceOperational, RulePrecedenceConfiguredOrder},
		{"un-nominated field while idle", FieldCustomerID, false, domain.SourceOperational, RulePrecedenceConfiguredOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newPolicy(map[string][]string{
				FieldStatus:     {"operational", "execution"},
				FieldCustomerID: {"operational", "execution"},
			}, []string{FieldStatus, FieldSubState})

			d := p.Resolve(tc.field, []Candidate{
				ops("from-operational"),
				exec("from-execution"),
			}, Context{ExecutionInProgress: tc.inProgress, InFlightExecutionID: "E-1"})

			testutil.Equal(t, d.Winner, tc.wantWinner, "winner")
			testutil.Equal(t, d.Rule, tc.wantRule, "rule id")
		})
	}
}

// TestResolve_ExecutionOverrideNeedsAnExecutionCandidate verifies REQ-PREC-005
// against the override rule: a running execution that did not supply a value
// must not blank the field. The override is a precedence change, not a licence
// to return nothing.
func TestResolve_ExecutionOverrideNeedsAnExecutionCandidate(t *testing.T) {
	t.Parallel()

	p, _ := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, []string{FieldStatus})

	d := p.Resolve(FieldStatus, []Candidate{
		ops(domain.StatusActive),
		{Source: domain.SourceCache, Present: true, Value: domain.StatusActive},
	}, Context{ExecutionInProgress: true})

	testutil.Equal(t, d.Winner, domain.SourceOperational,
		"the execution source offered nothing, so the configured order still applies")
	testutil.Equal(t, d.Rule, RulePrecedenceConfiguredOrder, "rule id")
}

// TestResolve_RecencyIsNotAuthority verifies REQ-PREC-006, which is the reason
// this type exists at all. The execution candidate is ten minutes newer than
// the operational one and configuration ranks operational first; operational
// must win. The two sources' timestamps mean different things -- last poll
// versus workflow step completion -- so comparing them is a category error, and
// a "newest wins" tiebreaker would silently invert the declared precedence.
func TestResolve_RecencyIsNotAuthority(t *testing.T) {
	t.Parallel()

	p, _ := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

	older := Candidate{
		Source: domain.SourceOperational, Present: true,
		ObservedAt: base.Add(-10 * time.Minute), Value: domain.StatusActive,
	}
	muchNewer := Candidate{
		Source: domain.SourceExecution, Present: true,
		ObservedAt: base, Value: domain.StatusTerminated,
	}

	d := p.Resolve(FieldStatus, []Candidate{older, muchNewer}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceOperational,
		"a ten-minute-newer execution timestamp must not outrank the declared order")
	testutil.Equal(t, d.Value, any(domain.StatusActive), "the older, observed value is the one served")
	testutil.Equal(t, d.Rule, RulePrecedenceConfiguredOrder, "rule id")

	// The same pair with the order reversed proves the winner tracks
	// configuration rather than the timestamps, which did not change.
	rev, _ := newPolicy(map[string][]string{FieldStatus: {"execution", "operational"}}, nil)
	d = rev.Resolve(FieldStatus, []Candidate{older, muchNewer}, Context{})
	testutil.Equal(t, d.Winner, domain.SourceExecution, "configuration, not recency, moved the winner")
}

// TestResolve_ConflictOnlyWhenValuesDiffer verifies REQ-PREC-004: a conflict is
// two sources reporting *different* values. Two sources agreeing is the normal,
// healthy case and must not be counted or warned about.
func TestResolve_ConflictOnlyWhenValuesDiffer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		a, b     any
		conflict bool
	}{
		{"equal strings", "eu-west-1", "eu-west-1", false},
		{"differing strings", "eu-west-1", "us-east-1", true},
		{"equal resource statuses", domain.StatusActive, domain.StatusActive, false},
		{"differing resource statuses", domain.StatusActive, domain.StatusTerminated, true},
		{"equal execution statuses", domain.ExecRunning, domain.ExecRunning, false},
		{"differing execution statuses", domain.ExecRunning, domain.ExecFailed, true},
		{"equal ints", 7, 7, false},
		{"differing ints", 7, 9, true},
		{"equal float64s", 1.5, 1.5, false},
		{"differing float64s", 1.5, 2.5, true},
		{"equal bools", true, true, false},
		{"differing bools", true, false, true},
		{"both nil", nil, nil, false},
		{"nil against a value", nil, domain.StatusActive, true},
		{"a value against nil", domain.StatusActive, nil, true},
		// Same text, different Go types: these are not the same value, and
		// treating them as equal would hide a genuine mapping bug.
		{"same text, different types", "ACTIVE", domain.StatusActive, true},
		// Nothing else is compared. Reporting a conflict that is not one costs
		// a warning; hiding one costs an operator hours, so the comparison errs
		// towards reporting.
		{"a non-comparable type is treated as differing", []string{"a"}, []string{"a"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, log := newPolicy(map[string][]string{FieldStatus: {"operational", "execution"}}, nil)

			d := p.Resolve(FieldStatus, []Candidate{ops(tc.a), exec(tc.b)}, Context{})

			testutil.Equal(t, d.Conflict, tc.conflict, "conflict verdict")
			testutil.Equal(t, d.Winner, domain.SourceOperational, "the winner is unaffected by the verdict")
			if tc.conflict {
				testutil.Equal(t, log.calls, []string{FieldStatus + "=OPERATIONAL"},
					"a real disagreement is reported once, naming the winner")
			} else {
				testutil.Equal(t, len(log.calls), 0,
					"two sources agreeing is not a conflict and must not warn")
			}
		})
	}
}

// TestResolve_RejectedNamesTheLosers verifies REQ-PREC-004: the decision
// carries the sources that lost, which is what lets a support engineer answer
// "where did this number come from, and what did we ignore?"
func TestResolve_RejectedNamesTheLosers(t *testing.T) {
	t.Parallel()

	p, _ := newPolicy(map[string][]string{FieldStatus: {"execution", "operational"}}, nil)

	d := p.Resolve(FieldStatus, []Candidate{
		ops(domain.StatusActive),
		exec(domain.StatusTerminated),
		{Source: domain.SourceCache, Present: true, Value: domain.StatusPending},
	}, Context{})

	testutil.Equal(t, d.Winner, domain.SourceExecution, "winner")
	testutil.Equal(t, d.Rejected, []domain.SourceKind{domain.SourceOperational, domain.SourceCache},
		"every present candidate that did not win is listed")
}

// TestOrder_ExposesTheConfiguredTable verifies REQ-PREC-002: the shipped
// default table reaches the policy exactly as the contract declares it, and
// unusable entries are dropped rather than being carried as a source that can
// never win.
func TestOrder_ExposesTheConfiguredTable(t *testing.T) {
	t.Parallel()

	p := NewPrecedence(config.Default().Precedence, nil)

	cases := map[string][]domain.SourceKind{
		FieldStatus:           {domain.SourceOperational, domain.SourceExecution},
		FieldConfiguration:    {domain.SourceOperational},
		FieldMetrics:          {domain.SourceOperational},
		FieldTopology:         {domain.SourceOperational},
		FieldOwner:            {domain.SourceOperational},
		FieldCustomerID:       {domain.SourceOperational, domain.SourceExecution},
		FieldLatestExecution:  {domain.SourceExecution},
		FieldExecutionHistory: {domain.SourceExecution},
		FieldLastOperation:    {domain.SourceExecution},
	}
	for field, want := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, p.Order(field), want, "configured order for %s", field)
		})
	}

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, len(p.Order("noSuchField")), 0, "an unconfigured field has no order")
	})

	t.Run("unparseable sources are dropped", func(t *testing.T) {
		t.Parallel()
		q, _ := newPolicy(map[string][]string{FieldStatus: {"operational", "wharrgarbl", "none", "execution"}}, nil)
		testutil.Equal(t, q.Order(FieldStatus),
			[]domain.SourceKind{domain.SourceOperational, domain.SourceExecution},
			"a source that names nothing routable cannot appear in the order")
	})
}

// TestOverridesWhenRunning verifies REQ-PREC-003: the override list is
// configuration, readable back out for the admin surface, and closed -- a field
// that is not listed is not subject to the rule.
func TestOverridesWhenRunning(t *testing.T) {
	t.Parallel()

	p := NewPrecedence(config.Default().Precedence, nil)

	cases := map[string]bool{
		FieldStatus:           true,
		FieldSubState:         true,
		FieldConfiguration:    false,
		FieldCustomerID:       false,
		FieldExecutionHistory: false,
		"noSuchField":         false,
	}
	for field, want := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, p.OverridesWhenRunning(field), want, "override membership for %s", field)
		})
	}
}

// TestWarnOnConflict verifies that the conflict-warning switch is configuration
// rather than a compiled-in choice, and that a nil callback is safe: a caller
// that does not want the metric must not have to supply a no-op.
func TestWarnOnConflict(t *testing.T) {
	t.Parallel()

	on := NewPrecedence(config.PrecedenceConfig{ConflictWarning: true}, nil)
	testutil.True(t, on.WarnOnConflict(), "warning enabled")

	off := NewPrecedence(config.PrecedenceConfig{ConflictWarning: false}, nil)
	testutil.False(t, off.WarnOnConflict(), "warning disabled")

	d := on.Resolve(FieldStatus, []Candidate{ops("a"), exec("b")}, Context{})
	testutil.True(t, d.Conflict, "resolving with a nil callback still detects the conflict")
}
