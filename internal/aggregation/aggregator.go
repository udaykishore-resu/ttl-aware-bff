// Package aggregation fans out to both data sources concurrently and merges
// what comes back into one canonical answer.
//
// The two things that make this more than a WaitGroup:
//
//   - Failure is expected and is not automatically fatal. Each source is
//     marked required or optional by the routing decision; an optional source
//     that fails produces a partial response with a warning, not a 500.
//   - Merging is not "last writer wins". Every field that both sources can
//     supply goes through the SourcePrecedencePolicy, and the winner is
//     recorded as provenance.
//
// Traceability: REQ-AGG-001..REQ-AGG-010, REQ-EDGE-006..REQ-EDGE-008.
package aggregation

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/policy"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// Task is one source call in a fan-out.
type Task struct {
	// Source identifies the data source, used for timeouts, warnings and
	// metrics.
	Source domain.SourceKind
	// Name is the operation name, used in traces.
	Name string
	// Required marks a source whose failure fails the whole request.
	Required bool
	// Timeout is this task's own budget. Zero means "use whatever the caller's
	// context already allows".
	Timeout time.Duration
	// Run performs the call.
	Run func(ctx context.Context) error
}

// TaskResult records one task's outcome.
type TaskResult struct {
	Source   domain.SourceKind
	Name     string
	Required bool
	Err      error
	Duration time.Duration
	// TimedOut distinguishes a task that ran out of its own budget from one
	// that failed for another reason.
	TimedOut bool
}

// FanOutResult is the outcome of a whole fan-out.
type FanOutResult struct {
	Results []TaskResult
	// Partial is true when at least one optional source failed.
	Partial bool
	// Elapsed is the wall time of the slowest task, which is the point of
	// running them concurrently.
	Elapsed time.Duration
	// Err is set only when a required source failed.
	Err error
}

// Hooks report fan-out events for metrics.
type Hooks struct {
	OnTask    func(source domain.SourceKind, name string, d time.Duration, err error)
	OnPartial func(source domain.SourceKind)
	OnElapsed func(d time.Duration)
}

// NoopHooks returns hooks that do nothing.
func NoopHooks() Hooks {
	return Hooks{
		OnTask:    func(domain.SourceKind, string, time.Duration, error) {},
		OnPartial: func(domain.SourceKind) {},
		OnElapsed: func(time.Duration) {},
	}
}

// FanOut runs every task concurrently and waits for all of them.
//
// It deliberately does NOT use errgroup's cancel-on-first-error behaviour. If
// the execution source fails at 50ms, cancelling the still-running operational
// call would throw away data the response could have used, and would turn a
// partial answer into no answer at all. Every task is allowed to finish or to
// exhaust its own budget (REQ-AGG-004).
//
// Each task gets its own derived context, so one source's timeout cannot
// cancel another's call.
func FanOut(ctx context.Context, tasks []Task, hooks Hooks) FanOutResult {
	if len(tasks) == 0 {
		return FanOutResult{}
	}
	start := time.Now()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]TaskResult, len(tasks))
	)

	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t Task) {
			defer wg.Done()

			taskCtx := ctx
			var cancel context.CancelFunc = func() {}
			if t.Timeout > 0 {
				taskCtx, cancel = context.WithTimeout(ctx, t.Timeout)
			}
			defer cancel()

			taskStart := time.Now()
			err := t.Run(taskCtx)
			d := time.Since(taskStart)

			res := TaskResult{
				Source:   t.Source,
				Name:     t.Name,
				Required: t.Required,
				Err:      err,
				Duration: d,
				TimedOut: err != nil && errs.CodeOf(err) == errs.CodeUpstreamTimeout,
			}
			mu.Lock()
			results[i] = res
			mu.Unlock()

			hooks.OnTask(t.Source, t.Name, d, err)
		}(i, t)
	}
	wg.Wait()

	out := FanOutResult{Results: results, Elapsed: time.Since(start)}
	hooks.OnElapsed(out.Elapsed)

	for _, r := range results {
		if r.Err == nil {
			continue
		}
		// A "not found" from an optional source is not a failure: it is an
		// answer, and it means the resource simply has no execution history.
		if !r.Required && errs.IsNotFound(r.Err) {
			continue
		}
		if r.Required {
			// The first required failure wins as the reported error, and tasks
			// are assembled in a stable order, so the reported error is
			// deterministic across identical failures.
			if out.Err == nil {
				out.Err = r.Err
			}
			continue
		}
		out.Partial = true
		hooks.OnPartial(r.Source)
	}
	return out
}

// ResultFor finds one task's result by source and name.
func (f FanOutResult) ResultFor(source domain.SourceKind, name string) (TaskResult, bool) {
	for _, r := range f.Results {
		if r.Source == source && r.Name == name {
			return r, true
		}
	}
	return TaskResult{}, false
}

// FailedSources lists the sources that did not answer.
func (f FanOutResult) FailedSources() []domain.SourceKind {
	seen := map[domain.SourceKind]bool{}
	var out []domain.SourceKind
	for _, r := range f.Results {
		if r.Err == nil || seen[r.Source] {
			continue
		}
		seen[r.Source] = true
		out = append(out, r.Source)
	}
	return out
}

// Warnings converts failed optional tasks into response warnings, so a partial
// answer always explains itself to the UI.
func (f FanOutResult) Warnings() []domain.Warning {
	var out []domain.Warning
	for _, r := range f.Results {
		if r.Err == nil || r.Required {
			continue
		}
		if errs.IsNotFound(r.Err) {
			continue
		}
		code := domain.WarnSourceUnavailable
		msg := "data from this source could not be retrieved and has been omitted"
		if r.TimedOut {
			code = domain.WarnSourceTimeout
			msg = "this source did not answer within its time budget and has been omitted"
		}
		out = append(out, domain.Warning{Code: code, Message: msg, Source: r.Source})
	}
	return out
}

// ---------------------------------------------------------------------------
// Merging
// ---------------------------------------------------------------------------

// Inputs is what the merger has to work with after a fan-out.
type Inputs struct {
	// Operational is the operational record, or nil if that source did not
	// answer.
	Operational *domain.Resource
	// OperationalFreshness is the evaluated freshness of that record.
	OperationalFreshness domain.Freshness
	// OperationalStale marks the operational record as past its TTL.
	OperationalStale bool

	// LatestExecution is the newest execution, or nil.
	LatestExecution *domain.Execution
	// History is the execution history page, or nil.
	History *domain.ExecutionList
}

// MergeOutput is the merged answer plus the record of how it was assembled.
type MergeOutput struct {
	Details    domain.ResourceDetails
	Provenance map[string]domain.SourceKind
	Conflicts  []policy.Decision
	// Warnings are merge-level warnings, distinct from fan-out warnings.
	Warnings []domain.Warning
}

// Merger applies the precedence policy to build one canonical view.
type Merger struct {
	prec *policy.SourcePrecedencePolicy
}

// NewMerger builds a merger over a precedence policy.
func NewMerger(p *policy.SourcePrecedencePolicy) *Merger { return &Merger{prec: p} }

// Merge assembles a ResourceDetails from whatever the sources returned.
//
// The interesting case is status. Both sources can offer one: the operational
// source observed it, the execution source predicts it from the workflow it
// just ran. Which wins is decided entirely by the precedence policy, including
// the "a running execution may override" rule. Nothing here hard-codes a
// preference (REQ-PREC-001, REQ-PREC-003).
func (m *Merger) Merge(in Inputs, tenantID, resourceID string) MergeOutput {
	out := MergeOutput{Provenance: map[string]domain.SourceKind{}}

	pctx := policy.Context{}
	if in.LatestExecution != nil && in.LatestExecution.Status.InProgress() {
		pctx.ExecutionInProgress = true
		pctx.InFlightExecutionID = in.LatestExecution.ExecutionID
	}
	// The operational record's in-flight marker names an execution; it does not
	// prove that execution is still running. Only an execution candidate that
	// is ACTUALLY in progress may claim the override -- otherwise a marker left
	// behind by a finished workflow would let a completed execution's predicted
	// status outrank the operational source's observed one, and /details would
	// disagree with /status, which is precisely what the in-flight resolution
	// exists to prevent (REQ-PREC-003).
	if in.Operational != nil && in.Operational.InFlightExecutionID != "" && pctx.InFlightExecutionID == "" {
		pctx.InFlightExecutionID = in.Operational.InFlightExecutionID
	}

	res := domain.Resource{TenantID: tenantID, ResourceID: resourceID}
	if in.Operational != nil {
		// Operational-only fields are copied wholesale: the catalogue says no
		// other source can supply them, so there is nothing to resolve.
		res = *in.Operational
		res.TenantID = tenantID
		res.ResourceID = resourceID
		for _, f := range []string{
			policy.FieldConfiguration, policy.FieldMetrics, policy.FieldTopology,
			policy.FieldOwner, policy.FieldLabels, policy.FieldType,
		} {
			out.Provenance[f] = domain.SourceOperational
		}
	}

	// status: contested.
	statusDecision := m.prec.Resolve(policy.FieldStatus, []policy.Candidate{
		{
			Source:     domain.SourceOperational,
			Present:    in.Operational != nil && in.Operational.Status != "" && in.Operational.Status != domain.StatusUnknown,
			ObservedAt: observedAt(in.Operational),
			Stale:      in.OperationalStale,
			Value:      statusOf(in.Operational),
		},
		{
			Source:     domain.SourceExecution,
			Present:    in.LatestExecution != nil && in.LatestExecution.ResourceStatusAfter != "" && in.LatestExecution.ResourceStatusAfter != domain.StatusUnknown,
			ObservedAt: updatedAt(in.LatestExecution),
			Value:      statusAfter(in.LatestExecution),
		},
	}, pctx)
	if !statusDecision.Winner.IsNone() {
		if v, ok := statusDecision.Value.(domain.ResourceStatus); ok {
			res.Status = v
		}
		out.Provenance[policy.FieldStatus] = statusDecision.Winner
	}
	if statusDecision.Conflict {
		out.Conflicts = append(out.Conflicts, statusDecision)
		if m.prec.WarnOnConflict() {
			out.Warnings = append(out.Warnings, domain.Warning{
				Code:    domain.WarnConflictResolved,
				Message: "the two sources reported different values for status; precedence policy selected " + string(statusDecision.Winner),
				Source:  statusDecision.Winner,
			})
		}
	}

	// subState: contested only when the override rule nominates it.
	subDecision := m.prec.Resolve(policy.FieldSubState, []policy.Candidate{
		{
			Source:  domain.SourceOperational,
			Present: in.Operational != nil && in.Operational.SubState != "",
			Stale:   in.OperationalStale,
			Value:   subStateOf(in.Operational),
		},
		{
			Source:  domain.SourceExecution,
			Present: in.LatestExecution != nil && in.LatestExecution.Operation != "",
			Value:   operationOf(in.LatestExecution),
		},
	}, pctx)
	if !subDecision.Winner.IsNone() {
		if v, ok := subDecision.Value.(string); ok && v != "" {
			res.SubState = v
		}
		out.Provenance[policy.FieldSubState] = subDecision.Winner
	}

	// customerId: either source may supply it; take the configured winner.
	custDecision := m.prec.Resolve(policy.FieldCustomerID, []policy.Candidate{
		{Source: domain.SourceOperational, Present: in.Operational != nil && in.Operational.CustomerID != "", Value: customerOf(in.Operational)},
		{Source: domain.SourceExecution, Present: in.LatestExecution != nil && in.LatestExecution.CustomerID != "", Value: customerOfExec(in.LatestExecution)},
	}, pctx)
	if !custDecision.Winner.IsNone() {
		if v, ok := custDecision.Value.(string); ok && v != "" {
			res.CustomerID = v
		}
		out.Provenance[policy.FieldCustomerID] = custDecision.Winner
	}
	if custDecision.Conflict {
		out.Conflicts = append(out.Conflicts, custDecision)
	}

	// Execution-only fields need no resolution.
	if in.LatestExecution != nil {
		out.Details.LatestExecution = in.LatestExecution
		out.Provenance[policy.FieldLatestExecution] = domain.SourceExecution
		if res.InFlightExecutionID == "" && in.LatestExecution.Status.InProgress() {
			res.InFlightExecutionID = in.LatestExecution.ExecutionID
		}
	}
	if in.History != nil && len(in.History.Items) > 0 {
		out.Details.ExecutionHistory = in.History.Items
		out.Provenance[policy.FieldExecutionHistory] = domain.SourceExecution
	}

	out.Details.Resource = res
	return out
}

// ---------------------------------------------------------------------------
// small accessors, written out so the candidate lists above stay readable
// ---------------------------------------------------------------------------

func statusOf(r *domain.Resource) any {
	if r == nil {
		return nil
	}
	return r.Status
}

func subStateOf(r *domain.Resource) any {
	if r == nil {
		return nil
	}
	return r.SubState
}

func customerOf(r *domain.Resource) any {
	if r == nil {
		return nil
	}
	return r.CustomerID
}

func observedAt(r *domain.Resource) time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.ObservedAt
}

func statusAfter(e *domain.Execution) any {
	if e == nil {
		return nil
	}
	return e.ResourceStatusAfter
}

func operationOf(e *domain.Execution) any {
	if e == nil {
		return nil
	}
	return e.Operation
}

func customerOfExec(e *domain.Execution) any {
	if e == nil {
		return nil
	}
	return e.CustomerID
}

func updatedAt(e *domain.Execution) time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.UpdatedAt
}

// Completeness reports how fully populated a merged answer is, which the
// application layer uses to decide whether to warn about partial data even
// when every source technically answered (REQ-EDGE-006).
func Completeness(d *domain.ResourceDetails, requested []string) float64 {
	if d == nil || len(requested) == 0 {
		return 0
	}
	present := 0
	for _, f := range requested {
		switch f {
		case policy.FieldStatus:
			if d.Status != "" && d.Status != domain.StatusUnknown {
				present++
			}
		case policy.FieldSubState:
			if d.SubState != "" {
				present++
			}
		case policy.FieldConfiguration:
			if len(d.Configuration) > 0 {
				present++
			}
		case policy.FieldMetrics:
			if len(d.Metrics) > 0 {
				present++
			}
		case policy.FieldTopology:
			if d.Topology != nil {
				present++
			}
		case policy.FieldOwner:
			if d.Owner != nil {
				present++
			}
		case policy.FieldLabels:
			if len(d.Labels) > 0 {
				present++
			}
		case policy.FieldCustomerID:
			if d.CustomerID != "" {
				present++
			}
		case policy.FieldLatestExecution:
			if d.LatestExecution != nil {
				present++
			}
		case policy.FieldExecutionHistory:
			if len(d.ExecutionHistory) > 0 {
				present++
			}
		default:
			present++
		}
	}
	return float64(present) / float64(len(requested))
}

// Ensure the datasource package is referenced from here, so that a change to
// the port signatures shows up as a compile error in the aggregator's tests
// rather than only at the wiring layer.
var _ = datasource.ReadOptions{}
