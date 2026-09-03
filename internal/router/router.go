// Package router decides which data source or sources answer a request.
//
// The decision is made by an ordered chain of named rules rather than by
// nested conditionals. That structure buys three things that matter in
// production:
//
//   - Every decision carries the id of the rule that made it, so a routing
//     surprise in production is one metric query away from an explanation.
//   - Rules can be reordered, added or disabled without touching the others.
//   - Each rule is a pure function of its inputs, so the whole policy is
//     testable as a truth table rather than through the HTTP surface.
//
// Traceability: REQ-RT-001..REQ-RT-012, REQ-TTL-001..REQ-TTL-009.
package router

import (
	"context"
	"strings"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
)

// Target is the set of sources a decision selects.
type Target int

const (
	// TargetNone means no source can serve the request.
	TargetNone Target = iota
	TargetOperational
	TargetExecution
	TargetBoth
)

// String renders the target for metrics, logs and the response envelope.
func (t Target) String() string {
	switch t {
	case TargetOperational:
		return "OPERATIONAL"
	case TargetExecution:
		return "EXECUTION"
	case TargetBoth:
		return "BOTH"
	default:
		return "NONE"
	}
}

// Includes reports whether a source participates in this target.
func (t Target) Includes(k domain.SourceKind) bool {
	switch t {
	case TargetOperational:
		return k == domain.SourceOperational
	case TargetExecution:
		return k == domain.SourceExecution
	case TargetBoth:
		return k == domain.SourceOperational || k == domain.SourceExecution
	}
	return false
}

// Rule identifiers. These strings are a public contract: they appear in
// metrics, traces and response metadata, and the specification documents them.
const (
	RuleTenantMissing      = "guard.tenant_missing"
	RuleBothUnavailable    = "health.both_unavailable"
	RuleFieldsExecOnly     = "fields.execution_only"
	RuleFieldsOpsOnly      = "fields.operational_only"
	RuleFieldsSpanBoth     = "fields.span_both"
	RuleStrongConsistency  = "consistency.strong_requires_operational"
	RulePrimaryUnavailable = "health.primary_unavailable"
	RuleTTLFresh           = "ttl.operational.fresh"
	RuleTTLStale           = "ttl.operational.stale"
	RuleTTLUnknown         = "ttl.unknown_freshness"
	RuleDefaultPreferred   = "default.preferred_source"
	// RuleUnconfigured marks a request type with no routing rule at all. It is
	// a deployment error rather than a routing outcome, and it is emitted so
	// that it is visible in the same metric as every other decision.
	RuleUnconfigured = "guard.unconfigured_request_type"
	// RuleFallbackAfterFailure is not part of the pre-flight chain: it is
	// stamped on a decision the application layer derives after the preferred
	// source has actually failed.
	RuleFallbackAfterFailure = "fallback.primary_failed"
)

// Health is the router's view of a source's usability.
type Health struct {
	Available bool
	Detail    string
}

// Request is the router's input: the classification plus everything the router
// needs that the classifier does not know about.
type Request struct {
	Classification classifier.Classification
	// OperationalHealth and ExecutionHealth come from the resilience layer.
	OperationalHealth Health
	ExecutionHealth   Health
	// Now is injected so routing is deterministic under test.
	Now time.Time
}

// Decision is the router's output. Everything downstream -- the aggregator,
// the adapters, the response builder -- reads its behaviour from this struct
// and nothing else.
type Decision struct {
	Target Target
	Rule   string
	Reason string

	// Primary is the source expected to answer; Fallback is what to try if it
	// cannot. For TargetBoth, Primary is the source whose failure is fatal.
	Primary  domain.SourceKind
	Fallback domain.SourceKind

	// OperationalTTL is the source freshness TTL applied to this request.
	OperationalTTL time.Duration
	// CacheTTL is the response cache lifetime. Reported separately from
	// OperationalTTL because they are different concepts (REQ-CACHE-001).
	CacheTTL time.Duration

	AllowStale bool
	MaxStale   time.Duration

	// RequiredSources marks which sources must succeed. A source present with
	// value false may fail, yielding a partial response instead of an error.
	RequiredSources map[domain.SourceKind]bool
	// PerSourceTimeout overrides the source's default call timeout.
	PerSourceTimeout map[domain.SourceKind]time.Duration

	// Freshness is the evaluation the decision was based on, carried forward
	// so the response envelope does not have to recompute it.
	Freshness freshness.Evaluation
	// ProbeFailed records that the freshness probe could not be completed.
	ProbeFailed bool

	Consistency classifier.Consistency
}

// RequiresSource reports whether a source's failure should fail the request.
func (d Decision) RequiresSource(k domain.SourceKind) bool {
	if d.RequiredSources == nil {
		return d.Target.Includes(k)
	}
	req, ok := d.RequiredSources[k]
	if !ok {
		return d.Target.Includes(k)
	}
	return req
}

// TimeoutFor returns the per-source timeout override, or zero for the default.
func (d Decision) TimeoutFor(k domain.SourceKind) time.Duration {
	if d.PerSourceTimeout == nil {
		return 0
	}
	return d.PerSourceTimeout[k]
}

// DataSourceRouter is the port the application layer depends on.
type DataSourceRouter interface {
	Select(ctx context.Context, req Request) (Decision, error)
}

// Hooks report routing events for metrics without importing observability.
type Hooks struct {
	OnDecision func(target Target, rule, requestType string)
	OnTTL      func(hit bool, requestType string)
	OnFallback func(from, to domain.SourceKind, requestType string)
	OnProbeErr func(err error)
}

// NoopHooks returns hooks that do nothing.
func NoopHooks() Hooks {
	return Hooks{
		OnDecision: func(Target, string, string) {},
		OnTTL:      func(bool, string) {},
		OnFallback: func(domain.SourceKind, domain.SourceKind, string) {},
		OnProbeErr: func(error) {},
	}
}

// Router is the policy-driven implementation.
type Router struct {
	cfg     *config.Provider
	catalog *policy.FieldCatalog
	fresh   *freshness.Manager
	hooks   Hooks
	rules   []rule
}

// rule is one link in the decision chain. It returns handled=false to pass.
type rule struct {
	id string
	fn func(*evalState) (Decision, bool)
}

// evalState is the working set shared by the rules in one evaluation. It is
// built once so that, for example, the freshness probe happens at most once
// per request no matter how many rules consult it.
type evalState struct {
	r    *Router
	ctx  context.Context
	req  Request
	rule config.RoutingRule
	defs config.RoutingDefaults

	preferred domain.SourceKind
	fallback  domain.SourceKind
	// pinned records that a field-requirement rule fixed the source set, so
	// later rules must not reintroduce a fallback the fields forbid.
	pinned bool
	// ageUnverified records that no freshness measurement was taken, so any
	// staleness ceiling must be treated as unmet rather than as satisfied.
	ageUnverified bool

	// lazily evaluated
	freshnessDone bool
	freshness     freshness.Evaluation
	probeFailed   bool
}

// New builds the router with the standard rule chain.
func New(cfg *config.Provider, catalog *policy.FieldCatalog, fresh *freshness.Manager, hooks Hooks) *Router {
	r := &Router{cfg: cfg, catalog: catalog, fresh: fresh, hooks: hooks}
	r.rules = []rule{
		{RuleTenantMissing, ruleTenantMissing},
		{RuleBothUnavailable, ruleBothUnavailable},
		{RuleFieldsExecOnly, ruleFieldsExecOnly},
		{RuleFieldsOpsOnly, ruleFieldsOpsOnly},
		{RuleFieldsSpanBoth, ruleFieldsSpanBoth},
		{RuleStrongConsistency, ruleStrongConsistency},
		{RulePrimaryUnavailable, rulePrimaryUnavailable},
		{RuleTTLFresh, ruleTTLFresh},
		{RuleTTLStale, ruleTTLStale},
		{RuleTTLUnknown, ruleTTLUnknown},
		{RuleDefaultPreferred, ruleDefaultPreferred},
	}
	return r
}

// Select runs the rule chain and returns the first decision produced.
func (r *Router) Select(ctx context.Context, req Request) (Decision, error) {
	cfg := r.cfg.Get()
	rule, ok := cfg.ResolveRule(req.Classification.TenantID, req.Classification.Type)
	if !ok {
		d := r.finish(Decision{
			Target: TargetNone, Rule: RuleUnconfigured,
			Reason: "no routing rule is configured for request type " + req.Classification.Type,
		}, req, config.RoutingRule{}, cfg.Routing.Defaults, false)
		// Reported like any other decision. A deployment that has lost a
		// request type's routing rule must show up in routing_decision_total,
		// which is the one place an operator looks first.
		r.hooks.OnDecision(d.Target, d.Rule, req.Classification.Type)
		return d, nil
	}
	defs := cfg.ResolveDefaults(req.Classification.TenantID)

	st := &evalState{r: r, ctx: ctx, req: req, rule: rule, defs: defs}
	st.preferred, _ = domain.ParseSourceKind(rule.PreferredSource)
	st.fallback, _ = domain.ParseSourceKind(rule.Fallback)
	if req.Now.IsZero() {
		st.req.Now = time.Now()
	}

	for _, rl := range r.rules {
		d, handled := rl.fn(st)
		if !handled {
			continue
		}
		if d.Rule == "" {
			d.Rule = rl.id
		}
		out := r.finish(d, st.req, rule, defs, st.pinned)
		out.Freshness = st.freshness
		out.ProbeFailed = st.probeFailed
		r.hooks.OnDecision(out.Target, out.Rule, req.Classification.Type)
		return out, nil
	}

	// Unreachable: the last rule always returns a decision. Kept as a guard so
	// a future edit to the chain cannot silently produce a nil decision.
	out := r.finish(Decision{
		Target: TargetNone, Rule: RuleDefaultPreferred,
		Reason: "no routing rule matched",
	}, st.req, rule, defs, st.pinned)
	r.hooks.OnDecision(out.Target, out.Rule, req.Classification.Type)
	return out, nil
}

// finish fills in the parts of a Decision that come from configuration rather
// than from the rule that fired, so no rule has to remember to set them.
func (r *Router) finish(d Decision, req Request, rule config.RoutingRule, defs config.RoutingDefaults, pinned bool) Decision {
	d.OperationalTTL = rule.TTL.D()
	d.CacheTTL = rule.CacheTTL.D()
	d.Consistency = req.Classification.Consistency

	// Publish what the router ACTUALLY applied, not the raw per-type value: the
	// effective allowance ORs in the tenant default, and a consumer reading the
	// decision must see the same answer the rules used.
	d.AllowStale = rule.AllowStale || defs.StaleAllowed()
	d.MaxStale = rule.MaxStale.D()
	if d.MaxStale == 0 {
		d.MaxStale = defs.MaxStale.D()
	}
	// Strong consistency overrides any configured stale allowance. The
	// configuration validator already refuses the combination, but a tenant
	// override or a caller-tightened consistency can produce it at runtime.
	if req.Classification.Consistency == classifier.ConsistencyStrong {
		d.AllowStale = false
	}

	if len(rule.RequiredSources) > 0 {
		d.RequiredSources = make(map[domain.SourceKind]bool, len(rule.RequiredSources))
		for name, required := range rule.RequiredSources {
			if k, ok := domain.ParseSourceKind(name); ok {
				d.RequiredSources[k] = required
			}
		}
	}
	if len(rule.PerSourceTimeout) > 0 {
		d.PerSourceTimeout = make(map[domain.SourceKind]time.Duration, len(rule.PerSourceTimeout))
		for name, t := range rule.PerSourceTimeout {
			if k, ok := domain.ParseSourceKind(name); ok {
				d.PerSourceTimeout[k] = t.D()
			}
		}
	}
	// The configured fallback travels with every decision, whether or not the
	// rule that fired used it. The application layer needs it for the
	// *post-failure* fallback: pre-flight health routing only catches a source
	// the breaker already knows is unwell, and the first failures of an outage
	// necessarily arrive before that (REQ-RES-006).
	if d.Fallback.IsNone() && !pinned {
		if fb, ok := domain.ParseSourceKind(rule.Fallback); ok {
			d.Fallback = fb
		}
	}
	if d.Primary.IsNone() {
		switch d.Target {
		case TargetOperational:
			d.Primary = domain.SourceOperational
		case TargetExecution:
			d.Primary = domain.SourceExecution
		case TargetBoth:
			d.Primary = domain.SourceOperational
		}
	}
	return d
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// 1. A request without a resolved tenant cannot be routed. This is a guard,
// not a routing decision: it exists so that a bug in the auth middleware
// surfaces as a refusal rather than as a cross-tenant read.
func ruleTenantMissing(s *evalState) (Decision, bool) {
	if strings.TrimSpace(s.req.Classification.TenantID) != "" {
		return Decision{}, false
	}
	return Decision{
		Target: TargetNone,
		Rule:   RuleTenantMissing,
		Reason: "request has no resolved tenant context",
	}, true
}

// 2. Both sources unavailable. Serving stale operational data is still
// possible if the request type allows it and the BFF has a cached observation,
// but the router cannot know that here -- it reports NONE and lets the
// application layer consult the cache for a degraded answer.
func ruleBothUnavailable(s *evalState) (Decision, bool) {
	if s.req.OperationalHealth.Available || s.req.ExecutionHealth.Available {
		return Decision{}, false
	}
	return Decision{
		Target: TargetNone,
		Rule:   RuleBothUnavailable,
		Reason: "operational source is " + s.req.OperationalHealth.Detail +
			" and execution source is " + s.req.ExecutionHealth.Detail,
	}, true
}

// 3. Every required field can only come from the execution source. This
// terminates the chain: TTL semantics belong to the operational source, so
// there is nothing further to evaluate.
func ruleFieldsExecOnly(s *evalState) (Decision, bool) {
	only, ok := s.exclusiveSource()
	if !ok || only != domain.SourceExecution {
		return Decision{}, false
	}
	if !s.healthOf(domain.SourceExecution).Available {
		return Decision{
			Target: TargetNone,
			Rule:   RuleFieldsExecOnly,
			Reason: "the only source that can supply the requested fields is " +
				s.healthOf(domain.SourceExecution).Detail,
		}, true
	}
	// Pin before returning. The configured fallback is a source that cannot
	// supply these fields, and a fallback that cannot answer is not a fallback,
	// it is a wrong answer -- the same reasoning as rule 4, and the reason
	// finish() must not re-attach one here either.
	s.preferred = domain.SourceExecution
	s.fallback = domain.SourceNone
	s.pinned = true
	return Decision{
		Target:  TargetExecution,
		Rule:    RuleFieldsExecOnly,
		Reason:  "requested fields are supplied only by the execution source",
		Primary: domain.SourceExecution,
	}, true
}

// 4. Every required field can only come from the operational source.
//
// This rule PINS the source rather than terminating the chain, and the
// distinction matters. Terminating here would skip the TTL rules, and with them
// the max_stale ceiling — so a request type like resource_configuration, whose
// fields no other source holds, would happily serve a record of any age. The
// ceiling is a safety property, not an optimisation, and it must hold even when
// there is nowhere else to go.
//
// Pinning also removes the fallback: a fallback source that cannot supply the
// requested fields is not a fallback, it is a wrong answer. The TTL rules
// downstream therefore see preferred=operational, fallback=none, and will
// either serve the record, serve it as stale-but-permitted, or refuse.
func ruleFieldsOpsOnly(s *evalState) (Decision, bool) {
	only, ok := s.exclusiveSource()
	if !ok || only != domain.SourceOperational {
		return Decision{}, false
	}
	if !s.healthOf(domain.SourceOperational).Available {
		return Decision{
			Target: TargetNone,
			Rule:   RuleFieldsOpsOnly,
			Reason: "the only source that can supply the requested fields is " +
				s.healthOf(domain.SourceOperational).Detail,
		}, true
	}
	s.preferred = domain.SourceOperational
	s.fallback = domain.SourceNone
	s.pinned = true
	return Decision{}, false
}

// 5. The requested fields span both sources: fan out.
func ruleFieldsSpanBoth(s *evalState) (Decision, bool) {
	fields := s.req.Classification.RequiredFields
	if len(fields) == 0 {
		return Decision{}, false
	}
	needed := s.r.catalog.SourcesFor(fields)
	if !(needed[domain.SourceOperational] && needed[domain.SourceExecution]) {
		return Decision{}, false
	}
	// If one side is down, degrade to the healthy side rather than issuing a
	// call that is certain to fail; the aggregator will mark the response
	// partial.
	switch {
	case s.req.OperationalHealth.Available && s.req.ExecutionHealth.Available:
		return Decision{
			Target: TargetBoth, Rule: RuleFieldsSpanBoth,
			Reason:  "requested fields span both sources",
			Primary: domain.SourceOperational,
		}, true
	case s.req.OperationalHealth.Available:
		return Decision{
			Target: TargetOperational, Rule: RuleFieldsSpanBoth,
			Reason:  "requested fields span both sources but the execution source is " + s.req.ExecutionHealth.Detail,
			Primary: domain.SourceOperational,
		}, true
	default:
		return Decision{
			Target: TargetExecution, Rule: RuleFieldsSpanBoth,
			Reason:  "requested fields span both sources but the operational source is " + s.req.OperationalHealth.Detail,
			Primary: domain.SourceExecution,
		}, true
	}
}

// 6. Strong consistency forbids answering from an aged observation. The
// preferred source is read live; no TTL evaluation is performed at all, which
// is why this rule sits above the TTL rules.
func ruleStrongConsistency(s *evalState) (Decision, bool) {
	if s.req.Classification.Consistency != classifier.ConsistencyStrong {
		return Decision{}, false
	}
	target := s.preferred
	if target.IsNone() {
		target = domain.SourceOperational
	}
	if !s.healthOf(target).Available {
		return Decision{
			Target: TargetNone, Rule: RuleStrongConsistency,
			Reason: "strong consistency requires a live read but the " +
				strings.ToLower(string(target)) + " source is " + s.healthOf(target).Detail,
		}, true
	}
	return Decision{
		Target:  targetFor(target),
		Rule:    RuleStrongConsistency,
		Reason:  "request requires strong consistency; reading the authoritative source live",
		Primary: target,
	}, true
}

// 7. The preferred source is unhealthy. Use the configured fallback if there
// is a healthy one; otherwise report NONE so the application layer can decide
// whether a stale cached answer is acceptable.
func rulePrimaryUnavailable(s *evalState) (Decision, bool) {
	// "both" is a preferred_source but not a SourceKind, so it parses to None.
	// Reading the configured string rather than the parsed value is what keeps
	// the both-source branch below reachable.
	prefersBoth := strings.EqualFold(s.rule.PreferredSource, "both")
	target := s.preferred
	if target.IsNone() && !prefersBoth {
		return Decision{}, false
	}
	if prefersBoth {
		if s.req.OperationalHealth.Available && s.req.ExecutionHealth.Available {
			return Decision{}, false
		}
	} else if s.healthOf(target).Available {
		return Decision{}, false
	}

	if !s.fallback.IsNone() && s.healthOf(s.fallback).Available {
		s.r.hooks.OnFallback(target, s.fallback, s.req.Classification.Type)
		return Decision{
			Target:  targetFor(s.fallback),
			Rule:    RulePrimaryUnavailable,
			Reason:  "preferred source is " + s.healthOf(target).Detail + "; using configured fallback",
			Primary: s.fallback,
		}, true
	}

	// "both" with one side healthy degrades to that side.
	if prefersBoth {
		if s.req.OperationalHealth.Available {
			return Decision{
				Target: TargetOperational, Rule: RulePrimaryUnavailable,
				Reason:  "execution source is " + s.req.ExecutionHealth.Detail + "; serving the operational side only",
				Primary: domain.SourceOperational,
			}, true
		}
		if s.req.ExecutionHealth.Available {
			return Decision{
				Target: TargetExecution, Rule: RulePrimaryUnavailable,
				Reason:  "operational source is " + s.req.OperationalHealth.Detail + "; serving the execution side only",
				Primary: domain.SourceExecution,
			}, true
		}
	}

	detail := "unavailable"
	if !target.IsNone() {
		detail = s.healthOf(target).Detail
	}
	return Decision{
		Target: TargetNone,
		Rule:   RulePrimaryUnavailable,
		Reason: "preferred source is " + detail + " and no healthy fallback is configured",
	}, true
}

// 8. The operational copy is within its TTL: read it. This is the rule that
// makes the whole design worthwhile, because it is the one that avoids the
// slow source entirely (REQ-PERF-004).
func ruleTTLFresh(s *evalState) (Decision, bool) {
	if s.preferred != domain.SourceOperational {
		return Decision{}, false
	}
	ev := s.evaluateFreshness()
	if ev.State != domain.FreshnessFresh {
		return Decision{}, false
	}
	s.r.hooks.OnTTL(true, s.req.Classification.Type)
	return Decision{
		Target:  TargetOperational,
		Rule:    RuleTTLFresh,
		Reason:  "operational data is within its freshness TTL",
		Primary: domain.SourceOperational,
	}, true
}

// 9. The operational copy is past its TTL. Prefer the configured fallback; if
// there is none, or it is unhealthy, fall back to serving stale operational
// data when the request type permits it.
func ruleTTLStale(s *evalState) (Decision, bool) {
	if s.preferred != domain.SourceOperational {
		return Decision{}, false
	}
	ev := s.evaluateFreshness()
	if ev.State != domain.FreshnessStale {
		return Decision{}, false
	}
	s.r.hooks.OnTTL(false, s.req.Classification.Type)

	// Past the hard staleness ceiling nothing may be served from operational.
	// An unverified age counts as beyond it: the ceiling is a safety property,
	// and "we did not measure" is not evidence that it holds.
	beyondCeiling := s.maxStale() > 0 && (s.ageUnverified || ev.Age > s.maxStale())

	if !s.fallback.IsNone() && s.healthOf(s.fallback).Available {
		s.r.hooks.OnFallback(domain.SourceOperational, s.fallback, s.req.Classification.Type)
		return Decision{
			Target:  targetFor(s.fallback),
			Rule:    RuleTTLStale,
			Reason:  "operational data exceeded its freshness TTL; routing to the execution source",
			Primary: s.fallback,
		}, true
	}

	if s.allowStale() && !beyondCeiling && s.req.OperationalHealth.Available {
		return Decision{
			Target:  TargetOperational,
			Rule:    RuleTTLStale,
			Reason:  "operational data is stale and no fallback is available; serving stale data as permitted by policy",
			Primary: domain.SourceOperational,
		}, true
	}

	return Decision{
		Target: TargetNone,
		Rule:   RuleTTLStale,
		Reason: "operational data is stale, no fallback is available, and policy forbids serving stale data",
	}, true
}

// 10. Freshness could not be established. The configured default decides,
// which keeps a probe outage from becoming an API outage.
func ruleTTLUnknown(s *evalState) (Decision, bool) {
	if s.preferred != domain.SourceOperational {
		return Decision{}, false
	}
	ev := s.evaluateFreshness()
	if ev.State != domain.FreshnessUnknown {
		return Decision{}, false
	}
	choice, ok := domain.ParseSourceKind(s.defs.OnUnknownFreshness)
	switch {
	case !ok:
		// Unparseable configuration falls back to the safest usable option.
		choice = domain.SourceOperational
	case choice.IsNone():
		// An operator who wrote "none" chose to fail rather than guess, and
		// that is a different thing from having written nothing. Conflating
		// the two would silently give the most optimistic behaviour to the
		// tenant who explicitly opted out of guessing (REQ-TTL-006).
		return Decision{
			Target: TargetNone, Rule: RuleTTLUnknown,
			Reason: "freshness could not be established and policy is configured to fail rather than guess",
		}, true
	}

	// A field-requirement rule may have pinned the source set. Crossing that
	// pin would route to a source that cannot supply the requested fields.
	if s.pinned && choice != s.preferred {
		choice = s.preferred
	}

	if !s.healthOf(choice).Available {
		// The configured choice is down; try the other side before giving up,
		// unless the fields forbid it.
		other := domain.SourceExecution
		if choice == domain.SourceExecution {
			other = domain.SourceOperational
		}
		if !s.pinned && s.healthOf(other).Available {
			s.r.hooks.OnFallback(choice, other, s.req.Classification.Type)
			choice = other
		} else {
			return Decision{
				Target: TargetNone, Rule: RuleTTLUnknown,
				Reason: "freshness could not be established and no source that can supply the requested fields is available",
			}, true
		}
	}
	reason := "freshness could not be established; applying the configured default"
	if s.probeFailed {
		reason = "freshness probe failed; applying the configured default"
	}
	return Decision{
		Target:  targetFor(choice),
		Rule:    RuleTTLUnknown,
		Reason:  reason,
		Primary: choice,
	}, true
}

// 11. Terminal rule: the configured preferred source, unconditionally. Reached
// for request types whose preferred source is the execution source or both,
// where no TTL evaluation applies.
func ruleDefaultPreferred(s *evalState) (Decision, bool) {
	switch strings.ToLower(s.rule.PreferredSource) {
	case "both":
		return Decision{
			Target: TargetBoth, Rule: RuleDefaultPreferred,
			Reason:  "request type is configured to read both sources",
			Primary: domain.SourceOperational,
		}, true
	case "execution":
		return Decision{
			Target: TargetExecution, Rule: RuleDefaultPreferred,
			Reason:  "request type prefers the execution source",
			Primary: domain.SourceExecution,
		}, true
	default:
		return Decision{
			Target: TargetOperational, Rule: RuleDefaultPreferred,
			Reason:  "request type prefers the operational source",
			Primary: domain.SourceOperational,
		}, true
	}
}

// ---------------------------------------------------------------------------
// evalState helpers
// ---------------------------------------------------------------------------

// evaluateFreshness probes at most once per request and memoises the result.
func (s *evalState) evaluateFreshness() freshness.Evaluation {
	if s.freshnessDone {
		return s.freshness
	}
	s.freshnessDone = true

	ttl := s.rule.TTL.D()
	skew := s.defs.ClockSkewTolerance.D()

	// A request type with TTL 0 never accepts age-based satisfaction, so there
	// is nothing to learn from a probe: skip it and save the round trip.
	if ttl <= 0 {
		// No probe is issued: a request type that will not accept an age-based
		// answer cannot act on one. The consequence is that the age is unknown,
		// so a max_stale ceiling cannot be verified either -- rule 9 must
		// therefore refuse to serve stale rather than assume an age of zero
		// clears the ceiling (REQ-TTL-005).
		s.ageUnverified = true
		s.freshness = freshness.Evaluation{Freshness: domain.Freshness{
			State: domain.FreshnessStale, TTL: 0, EvaluatedAt: s.req.Now, Source: domain.SourceOperational,
		}}
		return s.freshness
	}
	if !s.defs.ProbesEnabled() || s.r.fresh == nil || !s.req.OperationalHealth.Available {
		s.freshness = freshness.Evaluation{Freshness: domain.Freshness{
			State: domain.FreshnessUnknown, TTL: ttl, EvaluatedAt: s.req.Now, Source: domain.SourceOperational,
		}}
		return s.freshness
	}

	ev, err := s.r.fresh.Assess(s.ctx, s.req.Classification.TenantID, s.req.Classification.ResourceID, ttl, skew)
	if err != nil {
		s.probeFailed = true
		s.r.hooks.OnProbeErr(err)
		ev.State = domain.FreshnessUnknown
		ev.TTL = ttl
		ev.EvaluatedAt = s.req.Now
	}
	s.freshness = ev
	return s.freshness
}

// exclusiveSource reports the single source that can supply every requested
// field, if there is one.
func (s *evalState) exclusiveSource() (domain.SourceKind, bool) {
	fields := s.req.Classification.RequiredFields
	if len(fields) == 0 {
		return domain.SourceNone, false
	}
	return s.r.catalog.ExclusiveTo(fields)
}

func (s *evalState) healthOf(k domain.SourceKind) Health {
	switch k {
	case domain.SourceOperational:
		return s.req.OperationalHealth
	case domain.SourceExecution:
		return s.req.ExecutionHealth
	default:
		return Health{Available: false, Detail: "UNCONFIGURED"}
	}
}

func (s *evalState) allowStale() bool {
	if s.req.Classification.Consistency == classifier.ConsistencyStrong {
		return false
	}
	if s.rule.AllowStale {
		return true
	}
	return s.defs.StaleAllowed()
}

func (s *evalState) maxStale() time.Duration {
	if s.rule.MaxStale > 0 {
		return s.rule.MaxStale.D()
	}
	return s.defs.MaxStale.D()
}

func targetFor(k domain.SourceKind) Target {
	switch k {
	case domain.SourceOperational:
		return TargetOperational
	case domain.SourceExecution:
		return TargetExecution
	default:
		return TargetNone
	}
}
