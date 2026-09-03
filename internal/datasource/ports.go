// Package datasource declares the ports the application layer depends on.
//
// The interfaces here are written in terms of the canonical domain model, not
// in terms of gRPC or REST. That is the dependency inversion that lets the
// operational source move from gRPC to anything else without a single change
// above this line.
//
// The adapters live in the sub-packages and are the only code in the service
// that knows a source's wire format exists.
//
// Traceability: REQ-DS-001..REQ-DS-012, REQ-ARCH-003.
package datasource

import (
	"context"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
)

// Health describes a source's current usability, as judged locally by the
// resilience layer plus whatever the source itself reports.
type Health struct {
	Available bool
	// Detail is a short machine-readable reason: HEALTHY, CIRCUIT_OPEN,
	// SATURATED, CIRCUIT_HALF_OPEN, UNCONFIGURED.
	Detail string
	// LastError is the most recent failure, for diagnostics only.
	LastError string
	// CheckedAt is when the judgement was made.
	CheckedAt time.Time
}

// FreshnessProbe is the cheap pre-fetch read that makes TTL routing possible
// without paying for a full record.
//
// It is a separate interface from OperationalRepository on purpose: the router
// depends only on this, so a source that cannot answer a probe cheaply can
// implement one interface and not the other, and the router degrades to
// post-fetch evaluation instead of breaking.
type FreshnessProbe interface {
	// ProbeFreshness returns the source's own view of when it last refreshed
	// the record, together with the source's current time so the caller can
	// correct for clock skew.
	//
	// A missing record is reported by found=false, not by an error: "the
	// resource does not exist" is an answer, and it must not open a circuit.
	ProbeFreshness(ctx context.Context, tenantID, resourceID string) (Observation, error)
}

// Observation is the result of a freshness probe.
type Observation struct {
	Found bool
	// LastUpdated is the source-reported refresh instant.
	LastUpdated time.Time
	// SourceTime is the source's clock at the moment it answered. Comparing
	// LastUpdated against SourceTime rather than against the BFF's clock is
	// what makes the age immune to skew between the two machines.
	SourceTime time.Time
	// Version is the source's record version, when it publishes one.
	Version uint64
	// RefreshSource names the mechanism that last wrote the record.
	RefreshSource string
}

// OperationalRepository is the port for the Operational Data Source: fast,
// current-state reads.
type OperationalRepository interface {
	FreshnessProbe

	// GetResource returns the full current-state record.
	GetResource(ctx context.Context, tenantID, resourceID string, opts ReadOptions) (*domain.Resource, domain.Freshness, error)

	// GetResourceState returns only status and sub-state. Separate from
	// GetResource because the /status endpoint has a tighter latency budget
	// and no need for configuration, metrics or topology.
	GetResourceState(ctx context.Context, tenantID, resourceID string) (*domain.Resource, domain.Freshness, error)

	// BatchGetResources reads several records in one round trip.
	BatchGetResources(ctx context.Context, tenantID string, resourceIDs []string) ([]domain.Resource, error)

	// Health reports current usability.
	Health(ctx context.Context) Health

	// Close releases connections.
	Close() error
}

// ExecutionRepository is the port for the Execution Data Source: slower,
// workflow, history and audit reads.
type ExecutionRepository interface {
	// GetLatestExecution returns the most recent execution for a resource, or
	// a NOT_FOUND error when the resource has never been operated on.
	GetLatestExecution(ctx context.Context, tenantID, resourceID string) (*domain.Execution, error)

	// GetExecution returns one execution by id.
	GetExecution(ctx context.Context, tenantID, executionID string, opts ReadOptions) (*domain.Execution, error)

	// ListExecutions returns a page of execution history, newest first.
	ListExecutions(ctx context.Context, tenantID, resourceID string, page PageRequest, opts ReadOptions) (*domain.ExecutionList, error)

	// Health reports current usability.
	Health(ctx context.Context) Health

	// Close releases connections.
	Close() error
}

// ReadOptions carries per-call knobs that are policy decisions rather than
// source configuration: the router computes them and the adapter obeys.
type ReadOptions struct {
	// Timeout overrides the source's default call timeout. Zero means default.
	Timeout time.Duration
	// IncludeAudit requests audit records. Gated by RBAC, so it is a parameter
	// rather than something the adapter decides.
	IncludeAudit bool
	// IncludeSteps requests workflow step detail.
	IncludeSteps bool
	// Fields is an optional projection; empty means the full record.
	Fields []string
}

// PageRequest is the pagination input for history reads.
type PageRequest struct {
	Limit  int
	Cursor string
}

// SourceSet is a small helper for reasoning about which sources are involved.
type SourceSet map[domain.SourceKind]bool

// NewSourceSet builds a set from a list.
func NewSourceSet(kinds ...domain.SourceKind) SourceSet {
	s := make(SourceSet, len(kinds))
	for _, k := range kinds {
		s[k] = true
	}
	return s
}

// Has reports membership.
func (s SourceSet) Has(k domain.SourceKind) bool { return s[k] }

// List returns the members in a stable order (operational before execution),
// so log lines and metric attributes are comparable across requests.
func (s SourceSet) List() []domain.SourceKind {
	out := make([]domain.SourceKind, 0, len(s))
	for _, k := range []domain.SourceKind{domain.SourceOperational, domain.SourceExecution, domain.SourceCache} {
		if s[k] {
			out = append(out, k)
		}
	}
	return out
}
