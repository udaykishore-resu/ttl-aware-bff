package mapper

import (
	"fmt"
	"strings"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// ExecutionSchemaVersion is the EDS contract version this build understands.
const ExecutionSchemaVersion = "eds.v1"

// ---------------------------------------------------------------------------
// EDS wire types
//
// These structs are the REST payload exactly as the Execution Data Source
// publishes it, including its own naming conventions (executedAt, wfSteps,
// outcomeCode) and its own status vocabulary. They live in the mapper package
// because nothing above the adapter is allowed to see them.
// ---------------------------------------------------------------------------

// EDSExecution is one execution record as returned by the EDS.
type EDSExecution struct {
	ExecutionID string `json:"executionId"`
	TenantID    string `json:"tenantId"`
	ResourceID  string `json:"resourceId"`
	CustomerID  string `json:"customerId"`

	Operation string `json:"operation"`
	// State uses the EDS vocabulary: PENDING/IN_PROGRESS/SUCCEEDED/ERRORED/
	// ABORTED/EXPIRED. Note that none of these tokens match the canonical set.
	State string `json:"state"`

	// ResultingResourceState is the EDS's opinion of what the resource looks
	// like after this execution. It is a candidate value only.
	ResultingResourceState string `json:"resultingResourceState"`

	StartedAt   string `json:"startedAt"`
	ExecutedAt  string `json:"executedAt"`
	CompletedAt string `json:"completedAt"`
	UpdatedAt   string `json:"updatedAt"`

	WFSteps []EDSStep   `json:"wfSteps"`
	Actions []EDSAction `json:"actions"`

	Outcome *EDSOutcome `json:"outcome"`
	Failure *EDSFailure `json:"failure"`
	Audit   []EDSAudit  `json:"auditTrail"`

	SchemaVersion string `json:"schemaVersion"`
}

// EDSStep is one workflow step.
type EDSStep struct {
	StepID      string      `json:"stepId"`
	Label       string      `json:"label"`
	Ordinal     int         `json:"ordinal"`
	State       string      `json:"state"`
	StartedAt   string      `json:"startedAt"`
	CompletedAt string      `json:"completedAt"`
	Attempt     int         `json:"attempt"`
	Failure     *EDSFailure `json:"failure"`
}

// EDSAction is one recorded side effect.
type EDSAction struct {
	ActionID   string `json:"actionId"`
	Kind       string `json:"kind"`
	TargetRef  string `json:"targetRef"`
	Result     string `json:"result"`
	ExecutedAt string `json:"executedAt"`
	ExecutedBy string `json:"executedBy"`
}

// EDSOutcome is the terminal result.
type EDSOutcome struct {
	OutcomeCode string            `json:"outcomeCode"`
	Narrative   string            `json:"narrative"`
	Attributes  map[string]string `json:"attributes"`
}

// EDSFailure is a structured failure.
type EDSFailure struct {
	FailureCode string `json:"failureCode"`
	Detail      string `json:"detail"`
	Transient   bool   `json:"transient"`
	AtStep      string `json:"atStep"`
}

// EDSAudit is one audit record.
type EDSAudit struct {
	Timestamp string `json:"timestamp"`
	Actor     string `json:"actor"`
	Event     string `json:"event"`
	Detail    string `json:"detail"`
}

// EDSExecutionPage is the paged list response.
type EDSExecutionPage struct {
	Items      []EDSExecution `json:"items"`
	TotalCount int            `json:"totalCount"`
	NextCursor string         `json:"nextCursor"`
}

// ---------------------------------------------------------------------------
// Enum mappings
// ---------------------------------------------------------------------------

// executionStatus is the complete EDS -> canonical status mapping. The EDS
// vocabulary deliberately shares no tokens with the canonical one, so that a
// missing mapping shows up as UNKNOWN in tests rather than accidentally
// working.
var executionStatus = map[string]domain.ExecutionStatus{
	"PENDING":     domain.ExecQueued,
	"QUEUED":      domain.ExecQueued,
	"IN_PROGRESS": domain.ExecRunning,
	"RUNNING":     domain.ExecRunning,
	"SUCCEEDED":   domain.ExecCompleted,
	"ERRORED":     domain.ExecFailed,
	"ABORTED":     domain.ExecCancelled,
	"EXPIRED":     domain.ExecTimedOut,
}

// MapExecutionStatus converts an EDS state token to the canonical vocabulary.
func MapExecutionStatus(s string) domain.ExecutionStatus {
	if v, ok := executionStatus[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return v
	}
	return domain.ExecUnknown
}

// resultingResourceStatus maps the EDS's opinion of the post-execution
// resource state into the canonical resource vocabulary. This is what makes
// the execution source a *candidate* supplier of resource status, which the
// precedence policy may or may not accept.
var resultingResourceStatus = map[string]domain.ResourceStatus{
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

// MapResultingResourceStatus converts the EDS post-execution state.
func MapResultingResourceStatus(s string) domain.ResourceStatus {
	if v, ok := resultingResourceStatus[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return v
	}
	return domain.StatusUnknown
}

// ---------------------------------------------------------------------------
// Mapper
// ---------------------------------------------------------------------------

// Execution maps EDS records into canonical executions.
type Execution struct {
	accepted         map[string]struct{}
	onSchemaMismatch func(got string)
}

// NewExecution builds the mapper.
func NewExecution(acceptedVersions []string, onSchemaMismatch func(string)) *Execution {
	m := &Execution{onSchemaMismatch: onSchemaMismatch}
	if len(acceptedVersions) > 0 {
		m.accepted = make(map[string]struct{}, len(acceptedVersions))
		for _, v := range acceptedVersions {
			m.accepted[v] = struct{}{}
		}
	}
	if m.onSchemaMismatch == nil {
		m.onSchemaMismatch = func(string) {}
	}
	return m
}

// One maps a single execution record.
func (m *Execution) One(src *EDSExecution, tenantID string) (*domain.Execution, error) {
	if src == nil {
		return nil, errs.New(errs.CodeUpstreamInvalidPayload, "execution source returned an empty record").
			WithSource(string(domain.SourceExecution))
	}
	if err := m.checkSchema(src.SchemaVersion); err != nil {
		return nil, err
	}
	if src.TenantID != "" && tenantID != "" && src.TenantID != tenantID {
		return nil, errs.New(errs.CodeTenantMismatch,
			"execution source returned a record belonging to another tenant").
			WithSource(string(domain.SourceExecution)).
			WithOp("mapper.execution.one")
	}
	if src.ExecutionID == "" {
		return nil, errs.New(errs.CodeUpstreamInvalidPayload,
			"execution record is missing its execution identifier").
			WithSource(string(domain.SourceExecution))
	}

	out := &domain.Execution{
		ExecutionID:         src.ExecutionID,
		TenantID:            firstNonEmpty(src.TenantID, tenantID),
		ResourceID:          src.ResourceID,
		CustomerID:          src.CustomerID,
		Operation:           src.Operation,
		Status:              MapExecutionStatus(src.State),
		ResourceStatusAfter: MapResultingResourceStatus(src.ResultingResourceState),
		StartedAt:           parseTime(src.StartedAt, src.ExecutedAt),
		CompletedAt:         parseTime(src.CompletedAt),
		UpdatedAt:           parseTime(src.UpdatedAt, src.CompletedAt, src.ExecutedAt, src.StartedAt),
		SchemaVersion:       src.SchemaVersion,
	}

	if len(src.WFSteps) > 0 {
		out.Steps = make([]domain.WorkflowStep, 0, len(src.WFSteps))
		for _, s := range src.WFSteps {
			if s.StepID == "" {
				continue
			}
			out.Steps = append(out.Steps, domain.WorkflowStep{
				ID:          s.StepID,
				Name:        s.Label,
				Sequence:    s.Ordinal,
				Status:      MapExecutionStatus(s.State),
				StartedAt:   parseTime(s.StartedAt),
				CompletedAt: parseTime(s.CompletedAt),
				Attempt:     s.Attempt,
				Error:       mapFailure(s.Failure),
			})
		}
	}
	if len(src.Actions) > 0 {
		out.Actions = make([]domain.Action, 0, len(src.Actions))
		for _, a := range src.Actions {
			if a.ActionID == "" {
				continue
			}
			out.Actions = append(out.Actions, domain.Action{
				ID:          a.ActionID,
				Type:        a.Kind,
				Target:      a.TargetRef,
				Outcome:     a.Result,
				PerformedAt: parseTime(a.ExecutedAt),
				PerformedBy: a.ExecutedBy,
			})
		}
	}
	if o := src.Outcome; o != nil && (o.OutcomeCode != "" || o.Narrative != "") {
		out.Result = &domain.ExecutionResult{
			Outcome: o.OutcomeCode,
			Summary: o.Narrative,
			Values:  copyMap(o.Attributes),
		}
	}
	out.Error = mapFailure(src.Failure)

	if len(src.Audit) > 0 {
		out.Audit = make([]domain.AuditEntry, 0, len(src.Audit))
		for _, a := range src.Audit {
			out.Audit = append(out.Audit, domain.AuditEntry{
				At:      parseTime(a.Timestamp),
				Actor:   a.Actor,
				Action:  a.Event,
				Details: a.Detail,
			})
		}
	}
	return out, nil
}

// Page maps a list response, dropping individually invalid records rather than
// failing the whole page. A single malformed history row must not blank an
// otherwise useful list; the caller is told how many were dropped so it can
// warn (REQ-EDGE-020).
func (m *Execution) Page(src *EDSExecutionPage, tenantID string) (*domain.ExecutionList, int, error) {
	if src == nil {
		return nil, 0, errs.New(errs.CodeUpstreamInvalidPayload, "execution source returned an empty page").
			WithSource(string(domain.SourceExecution))
	}
	out := &domain.ExecutionList{
		Items:      make([]domain.Execution, 0, len(src.Items)),
		Total:      src.TotalCount,
		NextCursor: src.NextCursor,
	}
	dropped := 0
	for i := range src.Items {
		e, err := m.One(&src.Items[i], tenantID)
		if err != nil {
			// A tenant mismatch inside a page is not a droppable record: it
			// means the source is returning another tenant's data and the
			// whole response is untrustworthy.
			if errs.CodeOf(err) == errs.CodeTenantMismatch {
				return nil, 0, err
			}
			dropped++
			continue
		}
		out.Items = append(out.Items, *e)
	}
	domain.SortExecutionsByRecency(out.Items)
	if len(out.Items) > 0 {
		out.ResourceID = out.Items[0].ResourceID
	}
	return out, dropped, nil
}

func (m *Execution) checkSchema(got string) error {
	if m.accepted == nil || got == "" {
		return nil
	}
	if _, ok := m.accepted[got]; ok {
		return nil
	}
	m.onSchemaMismatch(got)
	return errs.New(errs.CodeSchemaVersionMismatch,
		fmt.Sprintf("execution source declared unsupported schema version %q", got)).
		WithSource(string(domain.SourceExecution)).
		WithOp("mapper.execution.schema")
}

func mapFailure(f *EDSFailure) *domain.ExecutionError {
	if f == nil || (f.FailureCode == "" && f.Detail == "") {
		return nil
	}
	return &domain.ExecutionError{
		Code:      f.FailureCode,
		Message:   f.Detail,
		Retryable: f.Transient,
		Step:      f.AtStep,
	}
}

// parseTime accepts the first parseable RFC 3339 timestamp from the candidates.
// The EDS is inconsistent about which timestamp fields it populates, and
// guessing here beats scattering nil checks through the aggregator.
func parseTime(candidates ...string) time.Time {
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, c); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
