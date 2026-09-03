package policy

import (
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
)

// Candidate is one source's offer for a field.
type Candidate struct {
	Source domain.SourceKind
	// Present distinguishes "the source answered and the field is empty" from
	// "the source did not answer at all". Only a present candidate can win.
	Present bool
	// ObservedAt is when the supplying source last refreshed the value.
	ObservedAt time.Time
	// Stale marks a candidate whose source data is past its freshness TTL.
	Stale bool
	// Value is the candidate value. The precedence policy does not interpret
	// it; it only decides which candidate wins.
	Value any
}

// Decision records which candidate won a field and why. It is emitted as
// provenance so that a support engineer can answer "where did this number come
// from?" without reading the code.
type Decision struct {
	Field    string
	Winner   domain.SourceKind
	Rule     string
	Conflict bool
	// Rejected lists the candidates that lost, for debugging.
	Rejected []domain.SourceKind
	Value    any
}

// Precedence rule identifiers, emitted verbatim in provenance and metrics.
const (
	RulePrecedenceOnlyCandidate    = "precedence.only_candidate"
	RulePrecedenceConfiguredOrder  = "precedence.configured_order"
	RulePrecedenceExecutionRunning = "precedence.execution_overrides_running"
	RulePrecedenceFreshBeatsStale  = "precedence.fresh_beats_stale"
	RulePrecedenceNoCandidate      = "precedence.no_candidate"
)

// SourcePrecedencePolicy resolves per-field conflicts between sources.
//
// The resolution order is fixed and, importantly, does NOT include "whichever
// timestamp is newer". Recency is not authority: the execution source can
// legitimately hold a newer timestamp for a status it is only *predicting*,
// while the operational source holds the older but observed truth. Choosing by
// recency would silently invert the intended precedence, which is exactly the
// implicit behaviour this type exists to prevent (REQ-PREC-002).
//
// Order:
//
//  1. No present candidate -> no winner.
//  2. Exactly one present candidate -> it wins.
//  3. An execution is in progress AND the field is listed in
//     execution_overrides_when_running -> the execution candidate wins.
//  4. The configured per-field order decides, skipping stale candidates while
//     a non-stale one exists.
//  5. If every candidate is stale, the configured order decides anyway, and
//     the caller marks the response degraded.
type SourcePrecedencePolicy struct {
	order          map[string][]domain.SourceKind
	overrideFields map[string]struct{}
	warnOnConflict bool
	onConflict     func(field string, winner domain.SourceKind)
}

// NewPrecedence builds the policy from configuration.
func NewPrecedence(cfg config.PrecedenceConfig, onConflict func(string, domain.SourceKind)) *SourcePrecedencePolicy {
	p := &SourcePrecedencePolicy{
		order:          make(map[string][]domain.SourceKind, len(cfg.Fields)),
		overrideFields: make(map[string]struct{}, len(cfg.ExecutionOverridesWhenRunning)),
		warnOnConflict: cfg.ConflictWarning,
		onConflict:     onConflict,
	}
	for field, names := range cfg.Fields {
		kinds := make([]domain.SourceKind, 0, len(names))
		for _, n := range names {
			if k, ok := domain.ParseSourceKind(n); ok && !k.IsNone() {
				kinds = append(kinds, k)
			}
		}
		p.order[field] = kinds
	}
	for _, f := range cfg.ExecutionOverridesWhenRunning {
		p.overrideFields[f] = struct{}{}
	}
	if p.onConflict == nil {
		p.onConflict = func(string, domain.SourceKind) {}
	}
	return p
}

// Context carries the request-scoped facts a precedence decision may depend on.
type Context struct {
	// ExecutionInProgress is true when a workflow is currently mutating the
	// resource, as reported by either source.
	ExecutionInProgress bool
	// InFlightExecutionID identifies that workflow, for the audit trail.
	InFlightExecutionID string
}

// Resolve picks the winning candidate for one field.
func (p *SourcePrecedencePolicy) Resolve(field string, candidates []Candidate, pctx Context) Decision {
	present := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Present {
			present = append(present, c)
		}
	}

	switch len(present) {
	case 0:
		return Decision{Field: field, Winner: domain.SourceNone, Rule: RulePrecedenceNoCandidate}
	case 1:
		return Decision{Field: field, Winner: present[0].Source, Rule: RulePrecedenceOnlyCandidate, Value: present[0].Value}
	}

	conflict := p.differ(present)

	// Rule 3: a running execution may override, but only for fields that
	// configuration has explicitly nominated.
	if pctx.ExecutionInProgress {
		if _, ok := p.overrideFields[field]; ok {
			if c, found := pick(present, domain.SourceExecution); found {
				p.note(field, conflict, domain.SourceExecution)
				return Decision{
					Field: field, Winner: domain.SourceExecution, Rule: RulePrecedenceExecutionRunning,
					Conflict: conflict, Rejected: others(present, domain.SourceExecution), Value: c.Value,
				}
			}
		}
	}

	order := p.order[field]
	if len(order) == 0 {
		// No configured order: the first present candidate wins, deterministic
		// because the aggregator always assembles candidates in source order.
		p.note(field, conflict, present[0].Source)
		return Decision{
			Field: field, Winner: present[0].Source, Rule: RulePrecedenceConfiguredOrder,
			Conflict: conflict, Rejected: others(present, present[0].Source), Value: present[0].Value,
		}
	}

	// Rule 4: configured order, preferring non-stale candidates.
	if c, found := firstInOrder(present, order, true); found {
		rule := RulePrecedenceConfiguredOrder
		if anyStale(present) {
			rule = RulePrecedenceFreshBeatsStale
		}
		p.note(field, conflict, c.Source)
		return Decision{
			Field: field, Winner: c.Source, Rule: rule,
			Conflict: conflict, Rejected: others(present, c.Source), Value: c.Value,
		}
	}

	// Rule 5: everything is stale; configured order still decides.
	if c, found := firstInOrder(present, order, false); found {
		p.note(field, conflict, c.Source)
		return Decision{
			Field: field, Winner: c.Source, Rule: RulePrecedenceConfiguredOrder,
			Conflict: conflict, Rejected: others(present, c.Source), Value: c.Value,
		}
	}

	// The configured order names no source that actually answered.
	p.note(field, conflict, present[0].Source)
	return Decision{
		Field: field, Winner: present[0].Source, Rule: RulePrecedenceConfiguredOrder,
		Conflict: conflict, Rejected: others(present, present[0].Source), Value: present[0].Value,
	}
}

// WarnOnConflict reports whether conflicts should surface as response warnings.
func (p *SourcePrecedencePolicy) WarnOnConflict() bool { return p.warnOnConflict }

// Order exposes the configured precedence for a field, for the admin surface
// and for tests that assert configuration reached the policy.
func (p *SourcePrecedencePolicy) Order(field string) []domain.SourceKind { return p.order[field] }

// OverridesWhenRunning reports whether a field is subject to the running
// execution override.
func (p *SourcePrecedencePolicy) OverridesWhenRunning(field string) bool {
	_, ok := p.overrideFields[field]
	return ok
}

func (p *SourcePrecedencePolicy) note(field string, conflict bool, winner domain.SourceKind) {
	if conflict {
		p.onConflict(field, winner)
	}
}

// differ reports whether the present candidates actually disagree. Two sources
// offering the same value is not a conflict and must not be reported as one,
// or every /details response would carry a warning.
func (p *SourcePrecedencePolicy) differ(present []Candidate) bool {
	if len(present) < 2 {
		return false
	}
	first := present[0].Value
	for _, c := range present[1:] {
		if !equalValue(first, c.Value) {
			return true
		}
	}
	return false
}

// equalValue compares candidate values. Only comparable scalar kinds are
// examined; anything else is treated as different, which errs towards
// reporting a conflict rather than hiding one.
func equalValue(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case domain.ResourceStatus:
		bv, ok := b.(domain.ResourceStatus)
		return ok && av == bv
	case domain.ExecutionStatus:
		bv, ok := b.(domain.ExecutionStatus)
		return ok && av == bv
	case int:
		bv, ok := b.(int)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

func pick(cs []Candidate, k domain.SourceKind) (Candidate, bool) {
	for _, c := range cs {
		if c.Source == k {
			return c, true
		}
	}
	return Candidate{}, false
}

func firstInOrder(cs []Candidate, order []domain.SourceKind, skipStale bool) (Candidate, bool) {
	for _, want := range order {
		for _, c := range cs {
			if c.Source != want {
				continue
			}
			if skipStale && c.Stale {
				continue
			}
			return c, true
		}
	}
	return Candidate{}, false
}

func anyStale(cs []Candidate) bool {
	for _, c := range cs {
		if c.Stale {
			return true
		}
	}
	return false
}

func others(cs []Candidate, winner domain.SourceKind) []domain.SourceKind {
	out := make([]domain.SourceKind, 0, len(cs)-1)
	for _, c := range cs {
		if c.Source != winner {
			out = append(out, c.Source)
		}
	}
	return out
}
