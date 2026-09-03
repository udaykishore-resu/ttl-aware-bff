package aggregation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/policy"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

var base = time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)

// sleepTask returns a task that occupies its goroutine for d, honouring
// cancellation so that a per-task timeout can actually cut it short.
func sleepTask(source domain.SourceKind, name string, required bool, timeout, d time.Duration, done *atomic.Bool) Task {
	return Task{
		Source: source, Name: name, Required: required, Timeout: timeout,
		Run: func(ctx context.Context) error {
			select {
			case <-time.After(d):
				if done != nil {
					done.Store(true)
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// failing returns a task that fails immediately with err.
func failing(source domain.SourceKind, name string, required bool, err error) Task {
	return Task{
		Source: source, Name: name, Required: required,
		Run: func(context.Context) error { return err },
	}
}

// TestFanOut_RunsTasksConcurrently verifies REQ-AGG-001: both source calls
// start together. Running them in sequence would make the /details latency
// budget the sum of two sources rather than the slower of the two, which is the
// entire reason the fan-out exists.
func TestFanOut_RunsTasksConcurrently(t *testing.T) {
	t.Parallel()

	const each = 60 * time.Millisecond
	tasks := []Task{
		sleepTask(domain.SourceOperational, "get_resource", true, 0, each, nil),
		sleepTask(domain.SourceExecution, "latest_execution", false, 0, each, nil),
		sleepTask(domain.SourceExecution, "history", false, 0, each, nil),
		sleepTask(domain.SourceOperational, "metrics", false, 0, each, nil),
	}

	out := FanOut(context.Background(), tasks, NoopHooks())

	testutil.NoError(t, out.Err, "no task failed")
	testutil.Equal(t, len(out.Results), len(tasks), "one result per task")
	// Sequential execution would take four times `each`. The bound is generous
	// so a loaded machine cannot make this flaky, while still being far below
	// what serial execution could possibly achieve.
	testutil.True(t, out.Elapsed < 2*each,
		"four %s tasks must overlap; the fan-out took %s", each, out.Elapsed)
}

// TestFanOut_RequiredFailureIsAnError verifies REQ-AGG-004: a source the
// decision marked required is one the answer cannot be assembled without, so
// its failure is the fan-out's error.
func TestFanOut_RequiredFailureIsAnError(t *testing.T) {
	t.Parallel()

	boom := errs.New(errs.CodeUpstreamUnavailable, "operational source is down")
	out := FanOut(context.Background(), []Task{
		failing(domain.SourceOperational, "get_resource", true, boom),
		sleepTask(domain.SourceExecution, "latest_execution", false, 0, time.Millisecond, nil),
	}, NoopHooks())

	testutil.Error(t, out.Err, "a required source failed")
	testutil.ErrCode(t, out.Err, errs.CodeUpstreamUnavailable, "the taxonomy code is preserved")
	testutil.False(t, out.Partial, "a required failure is not a partial answer; there is no answer")
	testutil.Equal(t, len(out.Warnings()), 0,
		"a required failure is reported as an error, not as a warning on a 200")

	t.Run("a required NOT_FOUND is still a failure", func(t *testing.T) {
		t.Parallel()
		// The not-found exemption is for optional sources only: a missing
		// required record means the request cannot be answered.
		out := FanOut(context.Background(), []Task{
			failing(domain.SourceOperational, "get_resource", true, errs.ErrNotFound),
		}, NoopHooks())
		testutil.ErrCode(t, out.Err, errs.CodeNotFound, "the not-found reaches the caller")
	})
}

// TestFanOut_OptionalFailureIsPartial verifies REQ-AGG-004: an optional source
// that fails costs the response a field group and a warning, not a 500. This is
// what turns a partial outage into a partial answer.
func TestFanOut_OptionalFailureIsPartial(t *testing.T) {
	t.Parallel()

	var partialFor []domain.SourceKind
	hooks := NoopHooks()
	hooks.OnPartial = func(s domain.SourceKind) { partialFor = append(partialFor, s) }

	out := FanOut(context.Background(), []Task{
		sleepTask(domain.SourceOperational, "get_resource", true, 0, time.Millisecond, nil),
		failing(domain.SourceExecution, "latest_execution", false, errs.ErrCircuitOpen),
	}, hooks)

	testutil.NoError(t, out.Err, "an optional failure must not fail the request")
	testutil.True(t, out.Partial, "but the answer is marked partial")
	testutil.Equal(t, partialFor, []domain.SourceKind{domain.SourceExecution}, "and the source is reported")

	warnings := out.Warnings()
	testutil.Equal(t, len(warnings), 1, "the partial answer explains itself, got %v", warnings)
	testutil.Equal(t, warnings[0].Code, domain.WarnSourceUnavailable, "warning code")
	testutil.Equal(t, warnings[0].Source, domain.SourceExecution, "warning source")
	testutil.True(t, len(warnings[0].Message) > 0, "with a message the UI can show")
}

// TestFanOut_OptionalNotFoundIsAnAnswer verifies REQ-AGG-004: "this resource
// has no execution history" is a fact, not a failure. Treating it as one would
// mark every never-executed resource's response partial and warn about a
// perfectly complete answer.
func TestFanOut_OptionalNotFoundIsAnAnswer(t *testing.T) {
	t.Parallel()

	out := FanOut(context.Background(), []Task{
		sleepTask(domain.SourceOperational, "get_resource", true, 0, time.Millisecond, nil),
		failing(domain.SourceExecution, "history", false, errs.ErrNotFound),
	}, NoopHooks())

	testutil.NoError(t, out.Err, "not an error")
	testutil.False(t, out.Partial, "not a partial answer either")
	testutil.Equal(t, len(out.Warnings()), 0, "and nothing to warn about")

	// The per-task record still carries the outcome, so the merger knows the
	// field group is empty rather than unattempted (REQ-AGG-003).
	r, ok := out.ResultFor(domain.SourceExecution, "history")
	testutil.True(t, ok, "the task's own record is still available")
	testutil.ErrCode(t, r.Err, errs.CodeNotFound, "and says exactly what happened")
}

// TestFanOut_TimeoutsAreIndependent verifies REQ-AGG-002 and REQ-EDGE-013: each
// task carries its own budget and its own derived context. One source running
// out of time must not cancel another -- an errgroup-style cancel-on-first-error
// here would convert a benign optional timeout into a total outage.
func TestFanOut_TimeoutsAreIndependent(t *testing.T) {
	t.Parallel()

	var siblingCompleted atomic.Bool
	out := FanOut(context.Background(), []Task{
		// Optional, 20ms of budget, 200ms of work: this one cannot finish.
		sleepTask(domain.SourceExecution, "latest_execution", false, 20*time.Millisecond, 200*time.Millisecond, nil),
		// Required, no budget of its own, 60ms of work: this one must finish.
		sleepTask(domain.SourceOperational, "get_resource", true, 0, 60*time.Millisecond, &siblingCompleted),
	}, NoopHooks())

	timedOut, ok := out.ResultFor(domain.SourceExecution, "latest_execution")
	testutil.True(t, ok, "the timed-out task has a record")
	testutil.Error(t, timedOut.Err, "it did not answer")
	testutil.True(t, timedOut.TimedOut, "and its record distinguishes a timeout from any other failure")
	testutil.True(t, timedOut.Duration < 150*time.Millisecond,
		"the budget cut it short rather than letting it run to completion, took %s", timedOut.Duration)

	sibling, ok := out.ResultFor(domain.SourceOperational, "get_resource")
	testutil.True(t, ok, "the sibling has a record")
	testutil.NoError(t, sibling.Err, "the sibling was never cancelled")
	testutil.True(t, siblingCompleted.Load(),
		"the sibling ran to completion: one source's timeout must not cancel another's call")

	testutil.NoError(t, out.Err, "the required source succeeded, so the request succeeds")
	testutil.True(t, out.Partial, "with the optional source's contribution missing")
}

// TestFanOut_WarningsDistinguishTimeoutFromUnavailable verifies REQ-AGG-004 and
// REQ-EDGE-013: "did not answer in time" and "would not answer at all" lead to
// different operator responses, so they are different warning codes rather than
// one generic failure.
func TestFanOut_WarningsDistinguishTimeoutFromUnavailable(t *testing.T) {
	t.Parallel()

	out := FanOut(context.Background(), []Task{
		failing(domain.SourceExecution, "latest_execution", false, context.DeadlineExceeded),
		failing(domain.SourceOperational, "metrics", false, errs.ErrCircuitOpen),
		failing(domain.SourceExecution, "history", false, errs.ErrNotFound),
		sleepTask(domain.SourceOperational, "get_resource", true, 0, time.Millisecond, nil),
	}, NoopHooks())

	warnings := out.Warnings()
	testutil.Equal(t, len(warnings), 2, "one warning per failed optional task, got %v", warnings)
	testutil.Equal(t, warnings[0], domain.Warning{
		Code:    domain.WarnSourceTimeout,
		Message: warnings[0].Message,
		Source:  domain.SourceExecution,
	}, "a deadline becomes a timeout warning")
	testutil.Equal(t, warnings[1].Code, domain.WarnSourceUnavailable,
		"an open circuit becomes an unavailability warning")
	testutil.Equal(t, warnings[1].Source, domain.SourceOperational, "attributed to the right source")
	testutil.NotEqual(t, warnings[0].Message, warnings[1].Message,
		"the two conditions read differently to whoever is looking at the response")
}

// TestFanOut_FailedSources verifies REQ-AGG-003: the sources that did not
// answer are reported once each, in a stable order, so the degradation logic
// and the response metadata agree about who was missing.
func TestFanOut_FailedSources(t *testing.T) {
	t.Parallel()

	out := FanOut(context.Background(), []Task{
		failing(domain.SourceOperational, "get_resource", false, errs.ErrCircuitOpen),
		failing(domain.SourceExecution, "latest_execution", false, errs.ErrCircuitOpen),
		// A second failure from the same source must not be listed twice.
		failing(domain.SourceExecution, "history", false, errs.ErrCircuitOpen),
	}, NoopHooks())

	testutil.Equal(t, out.FailedSources(),
		[]domain.SourceKind{domain.SourceOperational, domain.SourceExecution},
		"each failed source is named once, in task order")

	t.Run("nothing failed", func(t *testing.T) {
		t.Parallel()
		ok := FanOut(context.Background(), []Task{
			sleepTask(domain.SourceOperational, "get_resource", true, 0, time.Millisecond, nil),
		}, NoopHooks())
		testutil.Equal(t, len(ok.FailedSources()), 0, "a clean fan-out names no failed source")
	})

	t.Run("an unknown task has no result", func(t *testing.T) {
		t.Parallel()
		_, found := out.ResultFor(domain.SourceOperational, "never_dispatched")
		testutil.False(t, found, "asking for a task that was not run reports absence")
	})
}

// TestFanOut_NoTasks verifies that a routing decision of NONE, which dispatches
// nothing, produces an empty successful result rather than a failure or a
// panic. The degradation ladder above it is what turns that into an answer.
func TestFanOut_NoTasks(t *testing.T) {
	t.Parallel()

	for name, tasks := range map[string][]Task{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := FanOut(context.Background(), tasks, NoopHooks())
			testutil.Equal(t, len(out.Results), 0, "no results")
			testutil.NoError(t, out.Err, "no error")
			testutil.False(t, out.Partial, "not partial")
			testutil.Equal(t, len(out.Warnings()), 0, "no warnings")
			testutil.Equal(t, len(out.FailedSources()), 0, "no failed sources")
		})
	}
}

// ---------------------------------------------------------------------------
// Merging
// ---------------------------------------------------------------------------

// merger builds a merger over the shipped precedence configuration, so the
// tests assert the behaviour the service actually deploys with.
func merger() *Merger {
	return NewMerger(policy.NewPrecedence(config.Default().Precedence, nil))
}

// operationalRecord is a fully populated operational answer.
func operationalRecord() *domain.Resource {
	return &domain.Resource{
		TenantID:      "acme",
		ResourceID:    "R1",
		CustomerID:    "C-1",
		Type:          "database",
		Status:        domain.StatusActive,
		SubState:      "steady",
		Owner:         &domain.Owner{ID: "team-1"},
		Configuration: map[string]string{"tier": "gold"},
		Metrics:       []domain.Metric{{Name: "cpu", Value: 0.4}},
		Topology:      &domain.Topology{Region: "eu-west-1"},
		Labels:        map[string]string{"env": "prod"},
		ObservedAt:    base,
	}
}

// TestMerge_OperationalOnly verifies REQ-AGG-003 and REQ-PREC-001: with one
// source answering there is no conflict to resolve, and every field is still
// attributed in provenance so a cache hit or a support question can be answered
// without re-reading the code.
func TestMerge_OperationalOnly(t *testing.T) {
	t.Parallel()

	out := merger().Merge(Inputs{Operational: operationalRecord()}, "acme", "R1")

	testutil.Equal(t, out.Details.Status, domain.StatusActive, "status")
	testutil.Equal(t, out.Details.SubState, "steady", "subState")
	testutil.Equal(t, out.Details.Configuration, map[string]string{"tier": "gold"}, "configuration")
	testutil.Equal(t, out.Details.CustomerID, "C-1", "customerId")
	testutil.True(t, out.Details.LatestExecution == nil, "no execution was supplied")
	testutil.Equal(t, len(out.Conflicts), 0, "one source cannot conflict with itself")
	testutil.Equal(t, len(out.Warnings), 0, "and there is nothing to warn about")

	for _, f := range []string{
		policy.FieldStatus, policy.FieldSubState, policy.FieldConfiguration,
		policy.FieldMetrics, policy.FieldTopology, policy.FieldOwner,
		policy.FieldLabels, policy.FieldType, policy.FieldCustomerID,
	} {
		testutil.Equal(t, out.Provenance[f], domain.SourceOperational, "provenance for %s", f)
	}

	t.Run("the tenant and resource identity comes from the request", func(t *testing.T) {
		t.Parallel()
		// The merged answer is addressed by what the caller asked for, not by
		// what a source echoed back, so a mis-keyed source record cannot
		// relabel a response as another tenant's.
		mixed := operationalRecord()
		mixed.TenantID, mixed.ResourceID = "someone-else", "R999"
		got := merger().Merge(Inputs{Operational: mixed}, "acme", "R1")
		testutil.Equal(t, got.Details.TenantID, "acme", "tenant")
		testutil.Equal(t, got.Details.ResourceID, "R1", "resource")
	})
}

// TestMerge_ExecutionOnly verifies REQ-PREC-001: when only the execution source
// answered, its status candidate is the only one, so it supplies the canonical
// status -- and provenance says so, because the same field served from the
// other source on the next request must be distinguishable.
func TestMerge_ExecutionOnly(t *testing.T) {
	t.Parallel()

	latest := &domain.Execution{
		ExecutionID: "E-1", ResourceID: "R1", CustomerID: "C-1",
		Operation: "upgrade", Status: domain.ExecCompleted,
		ResourceStatusAfter: domain.StatusActive,
		UpdatedAt:           base,
	}
	history := &domain.ExecutionList{ResourceID: "R1", Items: []domain.Execution{*latest}, Total: 1}

	out := merger().Merge(Inputs{LatestExecution: latest, History: history}, "acme", "R1")

	testutil.Equal(t, out.Details.Status, domain.StatusActive, "the only status candidate is used")
	testutil.Equal(t, out.Provenance[policy.FieldStatus], domain.SourceExecution, "status provenance")
	testutil.Equal(t, out.Details.SubState, "upgrade", "the execution's operation supplies the sub-state")
	testutil.Equal(t, out.Details.CustomerID, "C-1", "customerId came from the only source that had it")
	testutil.Equal(t, out.Provenance[policy.FieldCustomerID], domain.SourceExecution, "customerId provenance")
	testutil.False(t, out.Details.Status == "", "a merged answer from one source is still a complete answer")

	testutil.Equal(t, out.Provenance[policy.FieldConfiguration], domain.SourceKind(""),
		"a field no source supplied has no provenance entry")

	t.Run("execution-only fields are copied without resolution", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, out.Details.LatestExecution, latest, "the latest execution is carried through as-is")
		testutil.Equal(t, out.Provenance[policy.FieldLatestExecution], domain.SourceExecution, "provenance")
		testutil.Equal(t, out.Details.ExecutionHistory, history.Items, "history is carried through as-is")
		testutil.Equal(t, out.Provenance[policy.FieldExecutionHistory], domain.SourceExecution, "provenance")
	})

	t.Run("an empty history is not a supplied field", func(t *testing.T) {
		t.Parallel()
		got := merger().Merge(Inputs{
			LatestExecution: latest,
			History:         &domain.ExecutionList{ResourceID: "R1"},
		}, "acme", "R1")
		testutil.Equal(t, len(got.Details.ExecutionHistory), 0, "nothing to carry")
		testutil.Equal(t, got.Provenance[policy.FieldExecutionHistory], domain.SourceKind(""),
			"an empty page is not attributed as data the source supplied")
	})
}

// TestMerge_StatusConflictResolvedByPrecedence verifies REQ-PREC-001,
// REQ-PREC-004 and REQ-EDGE-009: when both sources offer a status and they
// disagree, the declared precedence decides, the loser is recorded, and the
// response says a choice was made. Nothing here hard-codes a preference.
func TestMerge_StatusConflictResolvedByPrecedence(t *testing.T) {
	t.Parallel()

	// The execution record is newer than the operational one, and still loses:
	// the shipped table ranks operational first for status (REQ-PREC-006).
	out := merger().Merge(Inputs{
		Operational: operationalRecord(),
		LatestExecution: &domain.Execution{
			ExecutionID: "E-1", Status: domain.ExecCompleted,
			ResourceStatusAfter: domain.StatusTerminated,
			UpdatedAt:           base.Add(time.Hour),
		},
	}, "acme", "R1")

	testutil.Equal(t, out.Details.Status, domain.StatusActive,
		"the operational source observed the status; the execution source only predicted it")
	testutil.Equal(t, out.Provenance[policy.FieldStatus], domain.SourceOperational, "provenance names the winner")

	testutil.Equal(t, len(out.Conflicts), 1, "the disagreement is recorded, got %v", out.Conflicts)
	testutil.Equal(t, out.Conflicts[0].Field, policy.FieldStatus, "conflicted field")
	testutil.Equal(t, out.Conflicts[0].Rejected, []domain.SourceKind{domain.SourceExecution}, "the loser is named")

	testutil.Equal(t, len(out.Warnings), 1, "and the response explains that a choice was made")
	testutil.Equal(t, out.Warnings[0].Code, domain.WarnConflictResolved, "warning code")
	testutil.Equal(t, out.Warnings[0].Source, domain.SourceOperational, "the warning names the winner")

	t.Run("agreeing sources produce no warning", func(t *testing.T) {
		t.Parallel()
		agreed := merger().Merge(Inputs{
			Operational: operationalRecord(),
			LatestExecution: &domain.Execution{
				ExecutionID: "E-1", Status: domain.ExecCompleted,
				ResourceStatusAfter: domain.StatusActive,
			},
		}, "acme", "R1")
		testutil.Equal(t, agreed.Details.Status, domain.StatusActive, "same answer either way")
		testutil.Equal(t, len(agreed.Conflicts), 0, "two sources agreeing is not a conflict")
		testutil.Equal(t, len(agreed.Warnings), 0, "so the response carries no warning")
	})

	t.Run("an execution that predicts nothing does not blank the status", func(t *testing.T) {
		t.Parallel()
		// REQ-PREC-005: an UNKNOWN or empty prediction is not a value.
		empty := merger().Merge(Inputs{
			Operational: operationalRecord(),
			LatestExecution: &domain.Execution{
				ExecutionID: "E-1", Status: domain.ExecCompleted,
				ResourceStatusAfter: domain.StatusUnknown,
			},
		}, "acme", "R1")
		testutil.Equal(t, empty.Details.Status, domain.StatusActive, "the populated value survives")
		testutil.Equal(t, len(empty.Conflicts), 0, "and nothing was in conflict")
	})
}

// TestMerge_RunningExecutionOverrides verifies REQ-PREC-003: mid-flight the
// operational record lags the workflow by design, so for the nominated fields
// the execution source becomes authoritative. The merged status has to reflect
// that, or the UI shows a resource as ACTIVE while it is visibly being rebuilt.
func TestMerge_RunningExecutionOverrides(t *testing.T) {
	t.Parallel()

	running := &domain.Execution{
		ExecutionID: "E-77", Status: domain.ExecRunning,
		Operation:           "upgrade",
		ResourceStatusAfter: domain.StatusPending,
		UpdatedAt:           base,
	}

	out := merger().Merge(Inputs{Operational: operationalRecord(), LatestExecution: running}, "acme", "R1")

	testutil.Equal(t, out.Details.Status, domain.StatusPending,
		"while an execution is running the execution source owns the nominated fields")
	testutil.Equal(t, out.Provenance[policy.FieldStatus], domain.SourceExecution, "status provenance")
	testutil.Equal(t, out.Details.SubState, "upgrade",
		"subState is nominated too, so the running operation is what the UI shows")
	testutil.Equal(t, out.Provenance[policy.FieldSubState], domain.SourceExecution, "subState provenance")

	// customerId is not nominated, so the running execution does not move it.
	testutil.Equal(t, out.Provenance[policy.FieldCustomerID], domain.SourceOperational,
		"an un-nominated field keeps its declared precedence even mid-execution")

	t.Run("the in-flight execution is identified", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, out.Details.InFlightExecutionID, "E-77",
			"the response names the workflow that is mutating the resource")
	})

	t.Run("a queued execution counts as in progress", func(t *testing.T) {
		t.Parallel()
		queued := *running
		queued.Status = domain.ExecQueued
		got := merger().Merge(Inputs{Operational: operationalRecord(), LatestExecution: &queued}, "acme", "R1")
		testutil.Equal(t, got.Provenance[policy.FieldStatus], domain.SourceExecution,
			"a queued workflow is already mutating the resource's future")
	})

	t.Run("a terminal execution does not override", func(t *testing.T) {
		t.Parallel()
		finished := *running
		finished.Status = domain.ExecCompleted
		got := merger().Merge(Inputs{Operational: operationalRecord(), LatestExecution: &finished}, "acme", "R1")
		testutil.Equal(t, got.Details.Status, domain.StatusActive, "the declared order applies again")
		testutil.Equal(t, got.Provenance[policy.FieldStatus], domain.SourceOperational, "status provenance")
		testutil.Equal(t, got.Details.InFlightExecutionID, "",
			"and nothing is in flight")
	})

	t.Run("a stale in-flight marker does not grant the override", func(t *testing.T) {
		t.Parallel()
		// The operational record names an execution as in flight, but that
		// execution has since finished. The marker is a pointer, not proof: a
		// completed execution's PREDICTED status must not outrank the
		// operational source's OBSERVED one, or /details would contradict
		// /status, which is the divergence the in-flight resolution exists to
		// close (REQ-PREC-003).
		res := operationalRecord()
		res.InFlightExecutionID = "E-88"
		got := merger().Merge(Inputs{
			Operational: res,
			LatestExecution: &domain.Execution{
				ExecutionID: "E-88", Status: domain.ExecCompleted,
				ResourceStatusAfter: domain.StatusPending,
			},
		}, "acme", "R1")
		testutil.Equal(t, got.Details.Status, domain.StatusActive,
			"the operational source observed the state; the finished execution only predicted it")
		testutil.Equal(t, got.Provenance[policy.FieldStatus], domain.SourceOperational, "status provenance")
		testutil.Equal(t, got.Details.InFlightExecutionID, "E-88",
			"the marker is still reported, so a caller can see what the source believes")
	})

	t.Run("a running execution named by the operational record does grant it", func(t *testing.T) {
		t.Parallel()
		res := operationalRecord()
		res.InFlightExecutionID = "E-88"
		got := merger().Merge(Inputs{
			Operational: res,
			LatestExecution: &domain.Execution{
				ExecutionID: "E-88", Status: domain.ExecRunning,
				ResourceStatusAfter: domain.StatusPending,
			},
		}, "acme", "R1")
		testutil.Equal(t, got.Details.Status, domain.StatusPending,
			"a workflow that really is running outranks the operational record")
		testutil.Equal(t, got.Provenance[policy.FieldStatus], domain.SourceExecution, "status provenance")
	})
}

// TestCompleteness verifies REQ-EDGE-006: a response where every source
// answered can still be missing field groups the caller asked for, and the
// application layer needs a number to decide whether to say so.
func TestCompleteness(t *testing.T) {
	t.Parallel()

	full := merger().Merge(Inputs{
		Operational: operationalRecord(),
		LatestExecution: &domain.Execution{
			ExecutionID: "E-1", Status: domain.ExecCompleted, ResourceStatusAfter: domain.StatusActive,
		},
		History: &domain.ExecutionList{Items: []domain.Execution{{ExecutionID: "E-0"}}},
	}, "acme", "R1")

	requested := []string{
		policy.FieldStatus, policy.FieldSubState, policy.FieldConfiguration, policy.FieldMetrics,
		policy.FieldTopology, policy.FieldOwner, policy.FieldLabels, policy.FieldCustomerID,
		policy.FieldLatestExecution, policy.FieldExecutionHistory,
	}
	testutil.Equal(t, Completeness(&full.Details, requested), 1.0,
		"every requested field group was populated")

	t.Run("a partially populated answer", func(t *testing.T) {
		t.Parallel()
		// Only the operational source answered, so the two execution field
		// groups the caller asked for are missing.
		partial := merger().Merge(Inputs{Operational: operationalRecord()}, "acme", "R1")
		testutil.Equal(t, Completeness(&partial.Details, requested), 0.8,
			"eight of the ten requested field groups are present")
	})

	t.Run("an empty answer", func(t *testing.T) {
		t.Parallel()
		empty := merger().Merge(Inputs{}, "acme", "R1")
		testutil.Equal(t, Completeness(&empty.Details, requested), 0.0, "nothing was populated")
	})

	t.Run("degenerate inputs", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, Completeness(nil, requested), 0.0, "no details is not a complete answer")
		testutil.Equal(t, Completeness(&full.Details, nil), 0.0,
			"with nothing requested there is no completeness to report")
	})
}

// TestMerge_EmptyInputs verifies REQ-EDGE-019: no source answering yields an
// empty but well-formed record addressed to the right resource, which the
// caller distinguishes from "not requested" through IsZero rather than through
// a nil pointer.
func TestMerge_EmptyInputs(t *testing.T) {
	t.Parallel()

	out := merger().Merge(Inputs{}, "acme", "R1")

	testutil.Equal(t, out.Details.TenantID, "acme", "tenant")
	testutil.Equal(t, out.Details.ResourceID, "R1", "resource")
	testutil.Equal(t, out.Details.Status, domain.ResourceStatus(""), "no status was supplied")
	testutil.Equal(t, len(out.Provenance), 0, "nothing is attributed to any source")
	testutil.Equal(t, len(out.Conflicts), 0, "and nothing was in conflict")
}
