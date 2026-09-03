// Package domain holds the canonical model the UI sees.
//
// The dependency rule: this package imports nothing from the rest of the
// service. No source types, no transport types, no config types. Every other
// package may depend on domain; domain depends on no one. That is what makes
// it possible to swap either data source without touching the API contract.
//
// Traceability: REQ-API-002, REQ-MAP-001, REQ-DS-010.
package domain

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Source identity
// ---------------------------------------------------------------------------

// SourceKind names where a value came from. It is part of the public response
// (as provenance) because the UI must be able to tell an operator *why* it is
// looking at a particular number, even though it never has to choose a source.
type SourceKind string

const (
	SourceNone        SourceKind = "NONE"
	SourceOperational SourceKind = "OPERATIONAL"
	SourceExecution   SourceKind = "EXECUTION"
	SourceCache       SourceKind = "CACHE"
)

// IsNone reports whether the value names no source.
//
// This exists because SourceKind's zero value is "" while the explicit
// "no source" constant is "NONE", so a bare `k == SourceNone` comparison
// silently misses an unset field. Every "is there a source here?" check goes
// through this method so that mistake cannot be made twice.
func (s SourceKind) IsNone() bool { return s == "" || s == SourceNone }

// Valid reports whether the value is one of the defined source kinds.
func (s SourceKind) Valid() bool {
	switch s {
	case SourceNone, SourceOperational, SourceExecution, SourceCache:
		return true
	}
	return false
}

// ParseSourceKind maps configuration strings ("operational") to a SourceKind.
func ParseSourceKind(s string) (SourceKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "operational", "ods":
		return SourceOperational, true
	case "execution", "eds":
		return SourceExecution, true
	case "cache":
		return SourceCache, true
	case "", "none":
		return SourceNone, true
	}
	return SourceNone, false
}

// ---------------------------------------------------------------------------
// Freshness
// ---------------------------------------------------------------------------

// FreshnessState is the outcome of evaluating an observation against a TTL.
type FreshnessState string

const (
	// FreshnessUnknown means the age could not be established (no observation
	// timestamp, or the freshness probe failed). It is NOT the same as stale:
	// routing treats it separately (REQ-TTL-006).
	FreshnessUnknown FreshnessState = "UNKNOWN"
	FreshnessFresh   FreshnessState = "FRESH"
	FreshnessStale   FreshnessState = "STALE"
)

// Freshness describes how old a piece of data is relative to the TTL that was
// applied to it. Age is always computed in the *source's* clock domain where
// the source reports its own current time, which is what makes clock skew
// between the BFF and a source a non-issue (REQ-EDGE-010).
type Freshness struct {
	State FreshnessState `json:"state"`
	// Age is how long ago the source last refreshed the record.
	Age time.Duration `json:"-"`
	// TTL is the threshold that was applied to produce State.
	TTL time.Duration `json:"-"`
	// ObservedAt is the source-reported last-update instant.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// EvaluatedAt is when the BFF made the freshness judgement.
	EvaluatedAt time.Time `json:"evaluatedAt,omitzero"`
	// Source is the data source the observation came from.
	Source SourceKind `json:"source,omitempty"`
	// SkewCorrected records that BFF/source clock disagreement was compensated.
	SkewCorrected bool `json:"skewCorrected,omitempty"`
	// Version is the source's monotonic record version, when it publishes one.
	Version uint64 `json:"version,omitempty"`
}

// IsFresh is a readability helper; it does not re-evaluate anything.
func (f Freshness) IsFresh() bool { return f.State == FreshnessFresh }

// AgeSeconds renders the age for the wire format.
func (f Freshness) AgeSeconds() float64 { return f.Age.Seconds() }

// TTLSeconds renders the applied TTL for the wire format.
func (f Freshness) TTLSeconds() float64 { return f.TTL.Seconds() }

// MarshalJSON emits Age and TTL as seconds.
//
// They are held as time.Duration internally, which encoding/json would render
// as an opaque nanosecond integer. The age is the single most useful number in
// the whole envelope -- it is what tells a UI whether to show a "last updated
// N seconds ago" badge -- so it gets an explicit, documented representation
// rather than whatever the default happens to be.
func (f Freshness) MarshalJSON() ([]byte, error) {
	type alias Freshness // avoids recursing into this method
	return json.Marshal(struct {
		alias
		AgeSeconds float64 `json:"ageSeconds"`
		TTLSeconds float64 `json:"ttlSeconds"`
	}{
		alias:      alias(f),
		AgeSeconds: f.Age.Seconds(),
		TTLSeconds: f.TTL.Seconds(),
	})
}

// UnmarshalJSON restores Age and TTL from seconds, so a cache entry written by
// one instance round-trips faithfully through another.
func (f *Freshness) UnmarshalJSON(b []byte) error {
	type alias Freshness
	var raw struct {
		alias
		AgeSeconds float64 `json:"ageSeconds"`
		TTLSeconds float64 `json:"ttlSeconds"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*f = Freshness(raw.alias)
	f.Age = time.Duration(raw.AgeSeconds * float64(time.Second))
	f.TTL = time.Duration(raw.TTLSeconds * float64(time.Second))
	return nil
}

// ---------------------------------------------------------------------------
// Resource — current operational state
// ---------------------------------------------------------------------------

// ResourceStatus is the canonical status vocabulary. Neither source uses these
// exact tokens; both are mapped explicitly (see internal/mapper).
type ResourceStatus string

const (
	StatusUnknown     ResourceStatus = "UNKNOWN"
	StatusPending     ResourceStatus = "PENDING"
	StatusActive      ResourceStatus = "ACTIVE"
	StatusSuspended   ResourceStatus = "SUSPENDED"
	StatusDegraded    ResourceStatus = "DEGRADED"
	StatusTerminating ResourceStatus = "TERMINATING"
	StatusTerminated  ResourceStatus = "TERMINATED"
	StatusError       ResourceStatus = "ERROR"
)

// Owner is the canonical ownership record.
type Owner struct {
	ID         string `json:"id,omitempty"`
	Type       string `json:"type,omitempty"` // team | user | service
	Email      string `json:"email,omitempty"`
	CostCentre string `json:"costCentre,omitempty"`
}

// Metric is a single current-value sample. History is out of scope for the
// BFF: the UI gets the latest sample only.
type Metric struct {
	Name      string    `json:"name"`
	Value     float64   `json:"value"`
	Unit      string    `json:"unit,omitempty"`
	SampledAt time.Time `json:"sampledAt,omitzero"`
}

// Topology is the canonical placement/relationship record.
type Topology struct {
	Region     string   `json:"region,omitempty"`
	Zone       string   `json:"zone,omitempty"`
	Cluster    string   `json:"cluster,omitempty"`
	Upstream   []string `json:"upstream,omitempty"`
	Downstream []string `json:"downstream,omitempty"`
}

// Resource is the canonical current-state view of a managed resource.
type Resource struct {
	TenantID   string `json:"tenantId"`
	ResourceID string `json:"resourceId"`
	CustomerID string `json:"customerId,omitempty"`
	Type       string `json:"type,omitempty"`

	Status   ResourceStatus `json:"status"`
	SubState string         `json:"subState,omitempty"`

	Owner         *Owner            `json:"owner,omitempty"`
	Configuration map[string]string `json:"configuration,omitempty"`
	Metrics       []Metric          `json:"metrics,omitempty"`
	Topology      *Topology         `json:"topology,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`

	// InFlightExecutionID is set when the owning source knows a workflow is
	// currently mutating this resource. It is the trigger for the
	// execution-overrides-operational precedence rule (REQ-PREC-003).
	InFlightExecutionID string `json:"inFlightExecutionId,omitempty"`

	// ObservedAt is the source-reported last refresh of this record.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// SchemaVersion is the contract version declared by the originating source.
	SchemaVersion string `json:"-"`
}

// IsZero reports whether nothing was populated. Used to distinguish "empty
// result" from "not requested" (REQ-EDGE-019).
func (r *Resource) IsZero() bool {
	return r == nil || (r.ResourceID == "" && r.Status == "" && len(r.Configuration) == 0 && len(r.Metrics) == 0)
}

// Completeness returns the fraction of the core fields that are populated. The
// aggregator uses it to detect partially populated source records
// (REQ-EDGE-006, REQ-EDGE-007).
func (r *Resource) Completeness() float64 {
	if r == nil {
		return 0
	}
	const total = 8
	present := 0
	if r.ResourceID != "" {
		present++
	}
	if r.CustomerID != "" {
		present++
	}
	if r.Type != "" {
		present++
	}
	if r.Status != "" && r.Status != StatusUnknown {
		present++
	}
	if r.Owner != nil && r.Owner.ID != "" {
		present++
	}
	if len(r.Configuration) > 0 {
		present++
	}
	if len(r.Metrics) > 0 {
		present++
	}
	if r.Topology != nil && r.Topology.Region != "" {
		present++
	}
	return float64(present) / float64(total)
}

// ---------------------------------------------------------------------------
// Execution — workflow, history and audit
// ---------------------------------------------------------------------------

// ExecutionStatus is the canonical execution vocabulary.
type ExecutionStatus string

const (
	ExecUnknown   ExecutionStatus = "UNKNOWN"
	ExecQueued    ExecutionStatus = "QUEUED"
	ExecRunning   ExecutionStatus = "RUNNING"
	ExecCompleted ExecutionStatus = "COMPLETED"
	ExecFailed    ExecutionStatus = "FAILED"
	ExecCancelled ExecutionStatus = "CANCELLED"
	ExecTimedOut  ExecutionStatus = "TIMED_OUT"
)

// Terminal reports whether the execution has reached an end state.
func (s ExecutionStatus) Terminal() bool {
	switch s {
	case ExecCompleted, ExecFailed, ExecCancelled, ExecTimedOut:
		return true
	}
	return false
}

// InProgress reports whether a workflow is actively mutating the resource.
func (s ExecutionStatus) InProgress() bool {
	return s == ExecQueued || s == ExecRunning
}

// WorkflowStep is one node of an execution's workflow.
type WorkflowStep struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	Sequence    int             `json:"sequence"`
	Status      ExecutionStatus `json:"status"`
	StartedAt   time.Time       `json:"startedAt,omitzero"`
	CompletedAt time.Time       `json:"completedAt,omitzero"`
	Attempt     int             `json:"attempt,omitempty"`
	Error       *ExecutionError `json:"error,omitempty"`
}

// Action is a discrete side effect performed during an execution.
type Action struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Target      string    `json:"target,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
	PerformedAt time.Time `json:"performedAt,omitzero"`
	PerformedBy string    `json:"performedBy,omitempty"`
}

// ExecutionResult is the terminal output of an execution.
type ExecutionResult struct {
	Outcome string            `json:"outcome"`
	Summary string            `json:"summary,omitempty"`
	Values  map[string]string `json:"values,omitempty"`
}

// ExecutionError is a structured failure record.
type ExecutionError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	Step      string `json:"step,omitempty"`
}

// AuditEntry is one audit-log record attached to an execution.
type AuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Details string    `json:"details,omitempty"`
}

// Execution is the canonical view of one workflow run against a resource.
type Execution struct {
	ExecutionID string `json:"executionId"`
	TenantID    string `json:"tenantId"`
	ResourceID  string `json:"resourceId"`
	CustomerID  string `json:"customerId,omitempty"`

	Operation string          `json:"operation"`
	Status    ExecutionStatus `json:"status"`

	Steps   []WorkflowStep   `json:"steps,omitempty"`
	Actions []Action         `json:"actions,omitempty"`
	Result  *ExecutionResult `json:"result,omitempty"`
	Error   *ExecutionError  `json:"error,omitempty"`
	Audit   []AuditEntry     `json:"audit,omitempty"`

	StartedAt   time.Time `json:"startedAt,omitzero"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	UpdatedAt   time.Time `json:"updatedAt,omitzero"`

	// ResourceStatusAfter is the resource status the execution believes it has
	// produced. It is a *candidate* for the canonical status, never applied
	// implicitly — SourcePrecedencePolicy decides (REQ-PREC-001).
	ResourceStatusAfter ResourceStatus `json:"-"`

	SchemaVersion string `json:"-"`
}

// SortExecutionsByRecency orders executions newest-first using the most
// specific timestamp available. Ordering is explicit rather than relying on
// source order, because the two sources do not guarantee the same ordering.
func SortExecutionsByRecency(execs []Execution) {
	sort.SliceStable(execs, func(i, j int) bool {
		return executionOrderKey(execs[i]).After(executionOrderKey(execs[j]))
	})
}

func executionOrderKey(e Execution) time.Time {
	switch {
	case !e.UpdatedAt.IsZero():
		return e.UpdatedAt
	case !e.CompletedAt.IsZero():
		return e.CompletedAt
	default:
		return e.StartedAt
	}
}

// ---------------------------------------------------------------------------
// Aggregate views
// ---------------------------------------------------------------------------

// ResourceDetails is the both-sources view returned by /details. It is the
// only canonical type that deliberately spans sources.
type ResourceDetails struct {
	Resource
	LatestExecution  *Execution  `json:"latestExecution,omitempty"`
	ExecutionHistory []Execution `json:"executionHistory,omitempty"`
}

// ExecutionList is the paged history view.
type ExecutionList struct {
	ResourceID string      `json:"resourceId"`
	Items      []Execution `json:"items"`
	Total      int         `json:"total"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// ---------------------------------------------------------------------------
// Response metadata
// ---------------------------------------------------------------------------

// Warning is a non-fatal condition the UI may surface. Warnings are how a
// partial or degraded answer explains itself without using an error status.
type Warning struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Source  SourceKind `json:"source,omitempty"`
}

// Well-known warning codes.
const (
	WarnSourceUnavailable = "SOURCE_UNAVAILABLE"
	WarnSourceTimeout     = "SOURCE_TIMEOUT"
	WarnStaleData         = "STALE_DATA"
	WarnPartialData       = "PARTIAL_DATA"
	WarnConflictResolved  = "CONFLICT_RESOLVED"
	WarnSchemaMismatch    = "SCHEMA_VERSION_MISMATCH"
	WarnClockSkew         = "CLOCK_SKEW_DETECTED"
	WarnCacheUnavailable  = "CACHE_UNAVAILABLE"
)

// CacheLayer names which cache tier served the response, if any.
type CacheLayer string

const (
	CacheNone CacheLayer = "NONE"
	CacheL1   CacheLayer = "L1"
	CacheL2   CacheLayer = "L2"
)

// CacheInfo is the cache section of the response envelope. It is reported
// separately from Freshness because cache TTL and source freshness TTL are
// different concepts and conflating them is the classic bug this design
// exists to avoid (REQ-CACHE-001).
type CacheInfo struct {
	Hit   bool       `json:"hit"`
	Layer CacheLayer `json:"layer"`
	AgeMS int64      `json:"ageMs,omitempty"`
}

// ResponseMeta is attached to every successful response.
type ResponseMeta struct {
	CorrelationID   string                `json:"correlationId"`
	RoutingDecision string                `json:"routingDecision"`
	RoutingRule     string                `json:"routingRule,omitempty"`
	Sources         []SourceKind          `json:"sources"`
	Freshness       Freshness             `json:"freshness"`
	Degraded        bool                  `json:"degraded"`
	Partial         bool                  `json:"partial"`
	Cache           CacheInfo             `json:"cache"`
	Provenance      map[string]SourceKind `json:"provenance,omitempty"`
	Warnings        []Warning             `json:"warnings,omitempty"`
	ElapsedMS       int64                 `json:"elapsedMs,omitempty"`
}

// AddWarning appends a warning, de-duplicating on (code, source).
func (m *ResponseMeta) AddWarning(w Warning) {
	for _, existing := range m.Warnings {
		if existing.Code == w.Code && existing.Source == w.Source {
			return
		}
	}
	m.Warnings = append(m.Warnings, w)
}

// AddSource records that a source contributed to the response.
func (m *ResponseMeta) AddSource(s SourceKind) {
	if s == SourceNone || s == "" {
		return
	}
	for _, existing := range m.Sources {
		if existing == s {
			return
		}
	}
	m.Sources = append(m.Sources, s)
}

// SetProvenance records which source supplied a canonical field.
func (m *ResponseMeta) SetProvenance(field string, s SourceKind) {
	if m.Provenance == nil {
		m.Provenance = map[string]SourceKind{}
	}
	m.Provenance[field] = s
}
