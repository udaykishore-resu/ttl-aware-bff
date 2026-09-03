// Package application orchestrates the request lifecycle. It is the only place
// where the classifier, the router, the cache, the adapters, the mappers, the
// aggregator and the precedence policy meet.
//
// Everything it does is expressed in terms of ports and canonical types, so
// this package contains no HTTP, no gRPC, no JSON tags and no source schemas.
// That is what makes the lifecycle testable without a server.
//
// Traceability: REQ-API-*, REQ-RT-*, REQ-AGG-*, REQ-CACHE-*, REQ-RES-007.
package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/aggregation"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/cache"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/router"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// RuleDegradeStaleCache is the rule id stamped on a response served from an
// expired cache entry because no source could answer. It is not a router rule:
// the router had already decided nothing could serve the request.
const RuleDegradeStaleCache = "degrade.stale_cache"

// Envelope is what every successful use case returns: the canonical payload
// plus the metadata that explains how it was obtained.
type Envelope struct {
	Data any
	Meta domain.ResponseMeta
}

// Service implements the use cases behind the v1 API.
type Service struct {
	cfg     *config.Provider
	router  router.DataSourceRouter
	ops     datasource.OperationalRepository
	execs   datasource.ExecutionRepository
	merger  *aggregation.Merger
	prec    *policy.SourcePrecedencePolicy
	catalog *policy.FieldCatalog
	cache   *cache.Manager
	obs     *observability.Provider
	log     *slog.Logger
	now     func() time.Time
}

// Deps groups the service's collaborators.
type Deps struct {
	Config      *config.Provider
	Router      router.DataSourceRouter
	Operational datasource.OperationalRepository
	Execution   datasource.ExecutionRepository
	Precedence  *policy.SourcePrecedencePolicy
	Catalog     *policy.FieldCatalog
	Cache       *cache.Manager
	Observer    *observability.Provider
	Logger      *slog.Logger
}

// New builds the service.
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	obs := d.Observer
	if obs == nil {
		obs = observability.NewNoopProvider()
	}
	return &Service{
		cfg:     d.Config,
		router:  d.Router,
		ops:     d.Operational,
		execs:   d.Execution,
		merger:  aggregation.NewMerger(d.Precedence),
		prec:    d.Precedence,
		catalog: d.Catalog,
		cache:   d.Cache,
		obs:     obs,
		log:     log,
		now:     time.Now,
	}
}

// WithClock injects a clock for deterministic tests.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// ---------------------------------------------------------------------------
// Use cases
// ---------------------------------------------------------------------------

// GetResource serves GET /resources/{id}.
func (s *Service) GetResource(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.resourceView(ctx, cls, viewFull)
}

// GetResourceStatus serves GET /resources/{id}/status.
func (s *Service) GetResourceStatus(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.resourceView(ctx, cls, viewStatus)
}

// GetResourceConfiguration serves GET /resources/{id}/configuration.
func (s *Service) GetResourceConfiguration(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.resourceView(ctx, cls, viewConfiguration)
}

// GetResourceDetails serves GET /resources/{id}/details: the both-source
// fan-out.
func (s *Service) GetResourceDetails(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.resourceView(ctx, cls, viewDetails)
}

// ListExecutions serves GET /resources/{id}/executions.
func (s *Service) ListExecutions(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.executionView(ctx, cls, false)
}

// GetExecution serves GET /resources/{id}/executions/{executionId}.
func (s *Service) GetExecution(ctx context.Context, cls classifier.Classification) (*Envelope, error) {
	return s.executionView(ctx, cls, true)
}

// view selects which projection of the resource the caller asked for.
type view int

const (
	viewFull view = iota
	viewStatus
	viewConfiguration
	viewDetails
)

func (v view) String() string {
	switch v {
	case viewStatus:
		return "status"
	case viewConfiguration:
		return "configuration"
	case viewDetails:
		return "details"
	default:
		return "full"
	}
}

// ---------------------------------------------------------------------------
// Resource-oriented lifecycle
// ---------------------------------------------------------------------------

func (s *Service) resourceView(ctx context.Context, cls classifier.Classification, v view) (*Envelope, error) {
	ctx, span := s.obs.StartSpan(ctx, "bff.usecase.resource",
		attribute.String(observability.AttrRequestType, cls.Type),
		attribute.String("view", v.String()),
	)
	defer span.End()

	key := s.cacheKey(cls, "")
	rule, _ := s.cfg.Get().ResolveRule(cls.TenantID, cls.Type)

	// Step 1: cache-aside. The loader below is the cache-miss path, and it is
	// where routing, fetching, mapping and merging happen.
	res, err := s.load(ctx, cls, key, rule.CacheTTL.D(), func(ctx context.Context) (*cache.Entry, error) {
		return s.loadResource(ctx, cls, v)
	})
	if err != nil {
		// Step 2 of the degradation ladder: every source refused, but a stale
		// cached answer may still be acceptable.
		if env, ok := s.serveStale(ctx, cls, key, err); ok {
			span.SetStatus(codes.Ok, "served stale after source failure")
			return env, nil
		}
		if errs.IsNotFound(err) {
			s.cache.SetNegative(ctx, key)
		}
		span.SetStatus(codes.Error, errs.CodeOf(err).String())
		return nil, err
	}
	if res.Entry == nil {
		return nil, errs.New(errs.CodeInternal, "the request produced no result").WithOp("application.resource")
	}
	if res.Entry.Negative {
		return nil, errs.ErrNotFound.WithOp("application.resource.negative_cache")
	}

	return s.envelopeFrom(ctx, res, cls), nil
}

// load performs the cache-aside lookup, except for strongly consistent
// requests, which bypass the cache READ entirely.
//
// This is a correctness requirement, not an optimisation. A caller that asked
// for strong consistency wants what the authoritative source says now; handing
// it a two-second-old cached answer while reporting the request as a live read
// is exactly the failure the level exists to rule out. The result is still
// WRITTEN to the cache, because other callers at weaker levels can use it
// (REQ-CACHE-006).
func (s *Service) load(ctx context.Context, cls classifier.Classification, key string, ttl time.Duration, loader cache.Loader) (cache.Result, error) {
	if cls.Consistency != classifier.ConsistencyStrong {
		return s.cache.GetOrLoad(ctx, key, ttl, loader)
	}
	entry, err := loader(ctx)
	if err != nil {
		return cache.Result{Layer: string(domain.CacheNone)}, err
	}
	s.cache.Store(ctx, key, entry, ttl)
	return cache.Result{Entry: entry, Layer: string(domain.CacheNone)}, nil
}

// loadResource is the cache-miss path: the request lifecycle proper.
func (s *Service) loadResource(ctx context.Context, cls classifier.Classification, v view) (*cache.Entry, error) {
	// Steps 10-13: evaluate freshness, evaluate source health, select source.
	decision, err := s.route(ctx, cls)
	if err != nil {
		return nil, err
	}

	meta := domain.ResponseMeta{
		CorrelationID:   correlation.CorrelationID(ctx),
		RoutingDecision: decision.Target.String(),
		RoutingRule:     decision.Rule,
	}

	if decision.Target == router.TargetNone {
		return nil, errs.New(errs.CodeNoSourceAvailable, "no data source can currently satisfy this request").
			WithOp("application.route").
			WithDetail("routing_rule", decision.Rule).
			WithDetail("reason", decision.Reason)
	}

	// Steps 14-18: fetch concurrently, map, aggregate.
	in, fan, err := s.fetch(ctx, cls, decision, v)
	fellBack := false
	fallbackFrom := domain.SourceNone
	if err != nil {
		// Pre-flight health routing only avoids a source the circuit breaker
		// already knows is unwell. The first failures of any outage arrive
		// before that, so the primary source failing here is expected, and the
		// configured fallback has to be tried at call time rather than only at
		// routing time (REQ-RES-006, REQ-EDGE-003).
		fb, ok := s.fallbackDecision(ctx, cls, decision, err)
		if !ok {
			return nil, err
		}
		primaryErr, failedSource := err, decision.Primary
		in, fan, err = s.fetch(ctx, cls, fb, v)
		if err != nil {
			// Report the ORIGINAL failure. The fallback's error describes a
			// source the caller never asked about, and it can be actively
			// misleading: a NOT_FOUND from the execution source means only that
			// the resource has no execution history, and returning it would
			// turn a transient operational outage into a cached 404 for a
			// resource that exists.
			s.log.LogAttrs(ctx, slog.LevelWarn, "fallback source also failed",
				slog.String("fallback_error", err.Error()))
			return nil, primaryErr
		}
		decision, fellBack, fallbackFrom = fb, true, failedSource
	}

	// Step 19: apply source precedence.
	merged := s.merger.Merge(in, cls.TenantID, cls.ResourceID)

	// A request that reached a source but found nothing is a 404, not an empty
	// 200. An empty body would make "resource does not exist" and "resource
	// exists but has no data" indistinguishable to the UI (REQ-EDGE-019).
	// Test the INPUTS, not the merged output. Merge stamps the tenant and
	// resource id onto its result before anything else, so the merged record is
	// never zero-valued and a guard written against it can never fire -- which
	// would let "every source answered with nothing" be served as a 200 with an
	// empty body (REQ-EDGE-019).
	if in.Operational == nil && in.LatestExecution == nil && (in.History == nil || len(in.History.Items) == 0) {
		return nil, errs.ErrNotFound.WithOp("application.resource.empty")
	}

	// Step 20-21: build the canonical payload and its freshness metadata.
	freshnessOut := s.reportedFreshness(in, decision)
	meta.Freshness = freshnessOut
	meta.Provenance = merged.Provenance
	meta.Partial = fan.Partial
	meta.Degraded = freshnessOut.State == domain.FreshnessStale && decision.Target == router.TargetOperational
	// A routing-time fallback is just as much a degradation as a call-time one:
	// the answer did not come from the source the request type prefers, and a
	// UI that shows a freshness badge deserves to know that.
	if decision.Rule == router.RulePrimaryUnavailable {
		meta.Degraded = true
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnSourceUnavailable,
			Message: "the preferred data source was unavailable; this answer came from the configured fallback source",
			// The warning names the source that FAILED. Naming the one that
			// answered would invert the meaning of the single field an operator
			// reads to find out which side broke.
			Source: otherThan(decision.Primary),
		})
	}
	if fellBack {
		meta.Degraded = true
		meta.RoutingRule = decision.Rule
		meta.RoutingDecision = decision.Target.String()
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnSourceUnavailable,
			Message: "the preferred data source could not be reached; this answer came from the configured fallback source",
			Source:  fallbackFrom,
		})
	}
	for _, k := range s.contributingSources(decision, in) {
		meta.AddSource(k)
	}
	for _, w := range fan.Warnings() {
		meta.AddWarning(w)
	}
	for _, w := range merged.Warnings {
		meta.AddWarning(w)
	}
	// Only claim staleness when staleness is what actually happened. A response
	// degraded because the preferred source was unreachable was answered by the
	// other source, and no operational record was read at all -- saying its copy
	// is too old would assert something this request never established.
	if freshnessOut.State == domain.FreshnessStale {
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnStaleData,
			Message: "the operational source's copy of this record is older than its freshness policy allows",
			Source:  domain.SourceOperational,
		})
	}
	if freshnessOut.SkewCorrected {
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnClockSkew,
			Message: "the data source's clock disagrees with this service's; the reported age has been corrected",
			Source:  domain.SourceOperational,
		})
	}
	// A fallback answer comes from a source that may not hold the requested
	// fields at all. Those fields are then simply absent, and a 200 would tell
	// the caller the body is complete when it is not (REQ-AGG-006).
	if !meta.Partial && s.targetMissesRequiredFields(decision, cls) {
		meta.Partial = true
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnPartialData,
			Message: "this answer came from a source that does not hold every requested field; the remainder has been omitted",
		})
	}
	if completeness := aggregation.Completeness(&merged.Details, cls.RequiredFields); completeness < 1 && !meta.Partial {
		meta.AddWarning(domain.Warning{
			Code:    domain.WarnPartialData,
			Message: "one or more requested fields were not populated by the source that owns them",
		})
	}

	payload := s.project(&merged.Details, v)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, "the response could not be encoded", err).
			WithOp("application.encode")
	}

	return &cache.Entry{
		Payload:     raw,
		StoredAt:    s.now(),
		Freshness:   freshnessOut,
		Sources:     meta.Sources,
		Provenance:  meta.Provenance,
		Warnings:    meta.Warnings,
		Partial:     meta.Partial,
		RoutingRule: decision.Rule,
		Degraded:    meta.Degraded,
	}, nil
}

// project narrows the merged view to what the endpoint promises. Building the
// full view and then narrowing keeps one merge path for every endpoint; the
// cost is a few fields discarded, the benefit is that /status and /details
// cannot disagree about a resource's status.
func (s *Service) project(d *domain.ResourceDetails, v view) any {
	switch v {
	case viewStatus:
		return statusView{
			TenantID:            d.TenantID,
			ResourceID:          d.ResourceID,
			Status:              d.Status,
			SubState:            d.SubState,
			InFlightExecutionID: d.InFlightExecutionID,
			ObservedAt:          d.ObservedAt,
		}
	case viewConfiguration:
		return configurationView{
			TenantID:      d.TenantID,
			ResourceID:    d.ResourceID,
			Configuration: d.Configuration,
			Labels:        d.Labels,
			ObservedAt:    d.ObservedAt,
		}
	case viewDetails:
		return d
	default:
		return d.Resource
	}
}

// statusView is the narrow /status payload.
type statusView struct {
	TenantID            string                `json:"tenantId"`
	ResourceID          string                `json:"resourceId"`
	Status              domain.ResourceStatus `json:"status"`
	SubState            string                `json:"subState,omitempty"`
	InFlightExecutionID string                `json:"inFlightExecutionId,omitempty"`
	ObservedAt          time.Time             `json:"observedAt,omitzero"`
}

// configurationView is the narrow /configuration payload.
type configurationView struct {
	TenantID      string            `json:"tenantId"`
	ResourceID    string            `json:"resourceId"`
	Configuration map[string]string `json:"configuration,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	ObservedAt    time.Time         `json:"observedAt,omitzero"`
}

// ---------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------

// fetch issues the source calls the decision selected, concurrently when the
// decision names both.
func (s *Service) fetch(ctx context.Context, cls classifier.Classification, d router.Decision, v view) (aggregation.Inputs, aggregation.FanOutResult, error) {
	ctx, span := s.obs.StartSpan(ctx, "bff.aggregate",
		attribute.String(observability.AttrRoutingTarget, d.Target.String()),
		attribute.String(observability.AttrRoutingRule, d.Rule),
	)
	defer span.End()

	var (
		in    aggregation.Inputs
		tasks []aggregation.Task
	)

	if d.Target.Includes(domain.SourceOperational) {
		tasks = append(tasks, aggregation.Task{
			Source:   domain.SourceOperational,
			Name:     "ods.read",
			Required: d.RequiresSource(domain.SourceOperational),
			Timeout:  d.TimeoutFor(domain.SourceOperational),
			Run: func(ctx context.Context) error {
				var (
					r   *domain.Resource
					f   domain.Freshness
					err error
				)
				// The narrow status read exists precisely so the tightest
				// endpoint does not pay for configuration, metrics and
				// topology it will discard.
				if v == viewStatus {
					r, f, err = s.ops.GetResourceState(ctx, cls.TenantID, cls.ResourceID)
				} else {
					r, f, err = s.ops.GetResource(ctx, cls.TenantID, cls.ResourceID, datasource.ReadOptions{
						Timeout: d.TimeoutFor(domain.SourceOperational),
					})
				}
				if err != nil {
					return err
				}
				in.Operational = r
				in.OperationalFreshness = f
				return nil
			},
		})
	}

	if d.Target.Includes(domain.SourceExecution) {
		wantLatest := v == viewDetails || v == viewFull || d.Target == router.TargetExecution
		if wantLatest {
			tasks = append(tasks, aggregation.Task{
				Source:   domain.SourceExecution,
				Name:     "eds.latest",
				Required: d.RequiresSource(domain.SourceExecution),
				Timeout:  d.TimeoutFor(domain.SourceExecution),
				Run: func(ctx context.Context) error {
					e, err := s.execs.GetLatestExecution(ctx, cls.TenantID, cls.ResourceID)
					if err != nil {
						return err
					}
					in.LatestExecution = e
					return nil
				},
			})
		}
		if v == viewDetails {
			tasks = append(tasks, aggregation.Task{
				Source:   domain.SourceExecution,
				Name:     "eds.history",
				Required: false, // history is never worth failing a details view for
				Timeout:  d.TimeoutFor(domain.SourceExecution),
				Run: func(ctx context.Context) error {
					list, err := s.execs.ListExecutions(ctx, cls.TenantID, cls.ResourceID,
						datasource.PageRequest{Limit: cls.Limit},
						datasource.ReadOptions{IncludeAudit: cls.IncludeAudit, Timeout: d.TimeoutFor(domain.SourceExecution)})
					if err != nil {
						return err
					}
					in.History = list
					return nil
				},
			})
		}
	}

	fan := aggregation.FanOut(ctx, tasks, s.fanHooks())
	if fan.Err != nil {
		span.SetStatus(codes.Error, errs.CodeOf(fan.Err).String())
		return in, fan, fan.Err
	}

	// Evaluate the freshness of what actually came back. The pre-fetch probe
	// informed the routing decision; this is the authoritative evaluation, and
	// it is what the response reports (REQ-TTL-004).
	defs := s.cfg.Get().ResolveDefaults(cls.TenantID)
	if in.Operational != nil {
		ev := freshness.EvaluateFreshness(in.OperationalFreshness, d.OperationalTTL, defs.ClockSkewTolerance.D(), s.now())
		in.OperationalFreshness = ev.Freshness
		in.OperationalStale = ev.State == domain.FreshnessStale
	}

	s.resolveInFlight(ctx, cls, d, defs, &in)
	return in, fan, nil
}

// resolveInFlight closes the gap between an operational-only read and a
// both-source read while a workflow is running.
//
// The precedence policy says a running execution may override operational
// state for nominated fields. That rule can only fire if an execution
// candidate exists, so without this step /status (operational only) and
// /details (both sources) would report different statuses for the same
// resource mid-workflow -- the UI would see the answer change depending on
// which screen it was on.
//
// The extra call is made only when the operational record itself says a
// workflow is in flight, so the common case pays nothing. It is best-effort:
// a failure leaves the operational answer standing, unchanged (REQ-PREC-003).
func (s *Service) resolveInFlight(ctx context.Context, cls classifier.Classification, d router.Decision, defs config.RoutingDefaults, in *aggregation.Inputs) {
	if !defs.InFlightResolutionEnabled() ||
		in.Operational == nil ||
		in.Operational.InFlightExecutionID == "" ||
		in.LatestExecution != nil ||
		d.Target.Includes(domain.SourceExecution) {
		return
	}
	if h := s.execs.Health(ctx); !h.Available {
		return
	}

	ctx, span := s.obs.StartSpan(ctx, "bff.resolve_in_flight",
		attribute.String("execution_id", in.Operational.InFlightExecutionID))
	defer span.End()

	timeout := defs.InFlightLookupTimeout.D()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e, err := s.execs.GetExecution(callCtx, cls.TenantID, in.Operational.InFlightExecutionID,
		datasource.ReadOptions{Timeout: timeout})
	if err != nil {
		span.SetStatus(codes.Ok, "in-flight execution lookup failed; operational answer stands")
		s.log.LogAttrs(ctx, slog.LevelDebug, "in-flight execution lookup failed; serving the operational answer",
			slog.String("execution_id", in.Operational.InFlightExecutionID),
			slog.String(observability.AttrErrorCode, string(errs.CodeOf(err))),
		)
		return
	}
	// Only an actually-running execution may override. A workflow that has
	// finished since the operational record was written has no claim on the
	// resource's current state.
	if e != nil && e.Status.InProgress() {
		in.LatestExecution = e
	}
}

func (s *Service) route(ctx context.Context, cls classifier.Classification) (router.Decision, error) {
	ctx, span := s.obs.StartSpan(ctx, "bff.route")
	defer span.End()

	req := router.Request{
		Classification:    cls,
		OperationalHealth: toRouterHealth(s.ops.Health(ctx)),
		ExecutionHealth:   toRouterHealth(s.execs.Health(ctx)),
		Now:               s.now(),
	}
	d, err := s.router.Select(ctx, req)
	if err != nil {
		span.SetStatus(codes.Error, "routing failed")
		return router.Decision{}, err
	}
	span.SetAttributes(
		attribute.String(observability.AttrRoutingTarget, d.Target.String()),
		attribute.String(observability.AttrRoutingRule, d.Rule),
		attribute.String(observability.AttrFreshness, string(d.Freshness.State)),
	)
	s.log.LogAttrs(ctx, slog.LevelDebug, "routing decision",
		slog.String(observability.AttrRequestType, cls.Type),
		slog.String(observability.AttrRoutingTarget, d.Target.String()),
		slog.String(observability.AttrRoutingRule, d.Rule),
		slog.String("reason", d.Reason),
	)
	return d, nil
}

// ---------------------------------------------------------------------------
// Execution-oriented lifecycle
// ---------------------------------------------------------------------------

func (s *Service) executionView(ctx context.Context, cls classifier.Classification, single bool) (*Envelope, error) {
	ctx, span := s.obs.StartSpan(ctx, "bff.usecase.execution",
		attribute.String(observability.AttrRequestType, cls.Type))
	defer span.End()

	sub := cls.ExecutionID
	variant := map[string]string{}
	if !single {
		variant["limit"] = itoa(cls.Limit)
		if cls.Cursor != "" {
			variant["cursor"] = cls.Cursor
		}
	}
	if cls.IncludeAudit {
		variant["audit"] = "1"
	}
	key := s.cacheKeyWith(cls, sub, variant)

	rule, _ := s.cfg.Get().ResolveRule(cls.TenantID, cls.Type)

	res, err := s.load(ctx, cls, key, rule.CacheTTL.D(), func(ctx context.Context) (*cache.Entry, error) {
		decision, err := s.route(ctx, cls)
		if err != nil {
			return nil, err
		}
		if decision.Target == router.TargetNone {
			return nil, errs.New(errs.CodeNoSourceAvailable, "no data source can currently satisfy this request").
				WithOp("application.execution.route").
				WithDetail("routing_rule", decision.Rule).
				WithDetail("reason", decision.Reason)
		}

		var payload any
		if single {
			e, err := s.execs.GetExecution(ctx, cls.TenantID, cls.ExecutionID, datasource.ReadOptions{
				IncludeAudit: cls.IncludeAudit,
				IncludeSteps: true,
				Timeout:      decision.TimeoutFor(domain.SourceExecution),
			})
			if err != nil {
				return nil, err
			}
			// A resource-scoped URL must not be able to read another
			// resource's execution, even inside the right tenant.
			if cls.ResourceID != "" && e.ResourceID != "" && e.ResourceID != cls.ResourceID {
				return nil, errs.ErrNotFound.WithOp("application.execution.resource_mismatch")
			}
			payload = e
		} else {
			list, err := s.execs.ListExecutions(ctx, cls.TenantID, cls.ResourceID,
				datasource.PageRequest{Limit: cls.Limit, Cursor: cls.Cursor},
				datasource.ReadOptions{
					IncludeAudit: cls.IncludeAudit,
					Timeout:      decision.TimeoutFor(domain.SourceExecution),
				})
			if err != nil {
				return nil, err
			}
			payload = list
		}

		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, "the response could not be encoded", err).
				WithOp("application.encode")
		}
		return &cache.Entry{
			Payload:  raw,
			StoredAt: s.now(),
			// Execution data carries no freshness TTL: it is a record of what
			// happened, not a cache of current state. Reporting UNKNOWN here is
			// honest, where reporting FRESH would imply a guarantee the source
			// does not make.
			Freshness:   domain.Freshness{State: domain.FreshnessUnknown, Source: domain.SourceExecution, EvaluatedAt: s.now()},
			Sources:     []domain.SourceKind{domain.SourceExecution},
			RoutingRule: decision.Rule,
		}, nil
	})
	if err != nil {
		if env, ok := s.serveStale(ctx, cls, key, err); ok {
			return env, nil
		}
		if errs.IsNotFound(err) {
			s.cache.SetNegative(ctx, key)
		}
		span.SetStatus(codes.Error, errs.CodeOf(err).String())
		return nil, err
	}
	if res.Entry == nil {
		return nil, errs.New(errs.CodeInternal, "the request produced no result").WithOp("application.execution")
	}
	if res.Entry.Negative {
		return nil, errs.ErrNotFound.WithOp("application.execution.negative_cache")
	}
	return s.envelopeFrom(ctx, res, cls), nil
}

// fallbackDecision produces a decision targeting the configured fallback
// source, or reports that no fallback is possible.
//
// It refuses in every case where falling back would be wrong rather than
// merely unhelpful: a client-caused failure (a 404 is not something a second
// source can fix), a request type with no configured fallback, a fallback that
// is itself unhealthy, and a strongly consistent request, which asked for one
// specific source's live answer and must not silently receive another's.
func (s *Service) fallbackDecision(ctx context.Context, cls classifier.Classification, d router.Decision, cause error) (router.Decision, bool) {
	if !errs.SourceUnusable(cause) {
		return router.Decision{}, false
	}
	if cls.Consistency == classifier.ConsistencyStrong {
		return router.Decision{}, false
	}
	fb := d.Fallback
	if fb.IsNone() || fb == d.Primary {
		return router.Decision{}, false
	}
	// A BOTH decision that failed on its required source has no meaningful
	// single-source fallback: the missing side is the required one.
	if d.Target == router.TargetBoth && fb == domain.SourceExecution {
		return router.Decision{}, false
	}

	var h datasource.Health
	switch fb {
	case domain.SourceOperational:
		h = s.ops.Health(ctx)
	case domain.SourceExecution:
		h = s.execs.Health(ctx)
	}
	if !h.Available {
		return router.Decision{}, false
	}

	out := d
	out.Rule = router.RuleFallbackAfterFailure
	out.Reason = "the preferred source failed at call time; using the configured fallback"
	out.Primary = fb
	out.Fallback = domain.SourceNone
	switch fb {
	case domain.SourceOperational:
		out.Target = router.TargetOperational
	case domain.SourceExecution:
		out.Target = router.TargetExecution
	}
	// The fallback source is now required: there is nothing left behind it.
	out.RequiredSources = map[domain.SourceKind]bool{fb: true}

	s.obs.Metrics.FallbackTotal.Add(ctx, 1, observability.MetricAttrs(
		attribute.String("from", string(d.Primary)),
		attribute.String("to", string(fb)),
		attribute.String(observability.AttrRequestType, cls.Type),
		attribute.String("trigger", "call_failure"),
	))
	s.log.LogAttrs(ctx, slog.LevelWarn, "preferred source failed; falling back",
		slog.String("from", string(d.Primary)),
		slog.String("to", string(fb)),
		slog.String(observability.AttrRequestType, cls.Type),
		slog.String(observability.AttrErrorCode, string(errs.CodeOf(cause))),
	)
	return out, true
}

// ---------------------------------------------------------------------------
// Degradation and envelope construction
// ---------------------------------------------------------------------------

// serveStale is the last rung of the degradation ladder. It is reached only
// when the sources could not answer, and it serves a logically expired cache
// entry rather than an error, clearly marked as degraded.
//
// It refuses in three cases: the failure is the caller's fault, the request
// type forbids stale answers, or the entry is older than the request type's
// staleness ceiling. Serving stale data must be a policy decision, never a
// convenience (REQ-RES-007, REQ-EDGE-005).
func (s *Service) serveStale(ctx context.Context, cls classifier.Classification, key string, cause error) (*Envelope, bool) {
	// The same predicate the call-time fallback uses, plus the router's own
	// refusal. The stale rung sits BELOW the fallback rung on the degradation
	// ladder, so anything that justified trying another source must also
	// justify serving an old answer when that source is gone too.
	if !errs.SourceUnusable(cause) && errs.CodeOf(cause) != errs.CodeNoSourceAvailable {
		return nil, false
	}
	cfg := s.cfg.Get()
	rule, ok := cfg.ResolveRule(cls.TenantID, cls.Type)
	if !ok || !rule.AllowStale {
		return nil, false
	}
	if cls.Consistency == classifier.ConsistencyStrong {
		return nil, false
	}

	entry, layer, ok := s.cache.GetStale(ctx, key)
	if !ok || entry == nil {
		return nil, false
	}

	now := s.now()
	fr := entry.EffectiveFreshness(now)
	maxStale := rule.MaxStale.D()
	if maxStale == 0 {
		maxStale = cfg.ResolveDefaults(cls.TenantID).MaxStale.D()
	}
	if maxStale > 0 && fr.Age > maxStale {
		return nil, false
	}

	// Carry forward everything the original answer explained about itself. A
	// stale-served response is exactly when a caller most needs the provenance
	// and the warnings, and dropping them here would silently turn a partial
	// answer back into an apparently complete one.
	meta := domain.ResponseMeta{
		CorrelationID:   correlation.CorrelationID(ctx),
		RoutingDecision: router.TargetNone.String(),
		RoutingRule:     RuleDegradeStaleCache,
		Sources:         append([]domain.SourceKind{domain.SourceCache}, entry.Sources...),
		Provenance:      entry.Provenance,
		Warnings:        entry.Warnings,
		Partial:         entry.Partial,
		Freshness:       fr,
		Degraded:        true,
		Cache:           domain.CacheInfo{Hit: true, Layer: domain.CacheLayer(layer), AgeMS: entry.Age(now).Milliseconds()},
	}
	meta.AddWarning(domain.Warning{
		Code:    domain.WarnStaleData,
		Message: "every data source is currently unavailable; this response was served from cache and may be out of date",
	})
	s.obs.Metrics.StaleResponses.Add(ctx, 1,
		observability.MetricAttrs(attribute.String(observability.AttrRequestType, cls.Type)))
	s.log.LogAttrs(ctx, slog.LevelWarn, "serving stale cached data after source failure",
		slog.String(observability.AttrRequestType, cls.Type),
		slog.String(observability.AttrErrorCode, string(errs.CodeOf(cause))),
		slog.Float64("age_seconds", fr.Age.Seconds()),
	)
	return &Envelope{Data: json.RawMessage(entry.Payload), Meta: meta}, true
}

// envelopeFrom rebuilds the response metadata for an answer, whether it came
// from the cache or from a live load. A cache hit re-reports the *aged*
// freshness, so a cached answer never claims to be fresher than it is.
func (s *Service) envelopeFrom(ctx context.Context, res cache.Result, cls classifier.Classification) *Envelope {
	now := s.now()
	entry := res.Entry

	meta := domain.ResponseMeta{
		CorrelationID:   correlation.CorrelationID(ctx),
		RoutingRule:     entry.RoutingRule,
		Sources:         entry.Sources,
		Provenance:      entry.Provenance,
		Warnings:        entry.Warnings,
		Partial:         entry.Partial,
		Freshness:       entry.EffectiveFreshness(now),
		Degraded:        entry.Degraded,
		Cache:           domain.CacheInfo{Hit: res.Hit, Layer: domain.CacheLayer(res.Layer)},
		RoutingDecision: routingDecisionFor(entry.Sources),
	}
	if res.Hit {
		meta.Cache.AgeMS = entry.Age(now).Milliseconds()
		meta.AddSource(domain.SourceCache)
		// An entry that has aged past its freshness TTL while sitting in the
		// cache is reported as stale and degraded, even though the cache hit
		// itself was perfectly valid (REQ-EDGE-011).
		if meta.Freshness.State == domain.FreshnessStale && !meta.Degraded {
			meta.Degraded = true
			meta.AddWarning(domain.Warning{
				Code:    domain.WarnStaleData,
				Message: "this response was served from cache and the underlying data is older than its freshness policy allows",
			})
		}
	}
	if meta.Freshness.TTL > 0 {
		s.obs.Metrics.DataFreshnessAge.Record(ctx, meta.Freshness.Age.Seconds(),
			observability.MetricAttrs(attribute.String(observability.AttrRequestType, cls.Type)))
	}
	meta.ElapsedMS = correlation.Elapsed(ctx, now).Milliseconds()
	return &Envelope{Data: json.RawMessage(entry.Payload), Meta: meta}
}

// reportedFreshness picks which source's freshness the response reports. Only
// the operational source has a meaningful freshness TTL; an execution-sourced
// answer reports UNKNOWN rather than borrowing the operational verdict.
func (s *Service) reportedFreshness(in aggregation.Inputs, d router.Decision) domain.Freshness {
	if in.Operational != nil {
		f := in.OperationalFreshness
		if f.TTL == 0 {
			f.TTL = d.OperationalTTL
		}
		f.Source = domain.SourceOperational
		return f
	}
	return domain.Freshness{
		State:       domain.FreshnessUnknown,
		TTL:         d.OperationalTTL,
		EvaluatedAt: s.now(),
		Source:      domain.SourceExecution,
	}
}

func (s *Service) contributingSources(d router.Decision, in aggregation.Inputs) []domain.SourceKind {
	var out []domain.SourceKind
	if in.Operational != nil {
		out = append(out, domain.SourceOperational)
	}
	if in.LatestExecution != nil || in.History != nil {
		out = append(out, domain.SourceExecution)
	}
	if len(out) == 0 && !d.Primary.IsNone() {
		out = append(out, d.Primary)
	}
	return out
}

func (s *Service) cacheKey(cls classifier.Classification, sub string) string {
	return s.cacheKeyWith(cls, sub, nil)
}

func (s *Service) cacheKeyWith(cls classifier.Classification, sub string, variant map[string]string) string {
	return cache.Key{
		Prefix:      s.cache.KeyPrefix(),
		TenantID:    cls.TenantID,
		RequestType: cls.Type,
		ResourceID:  cls.ResourceID,
		Sub:         sub,
		Variant:     variant,
	}.String()
}

func (s *Service) fanHooks() aggregation.Hooks {
	return aggregation.Hooks{
		OnTask: func(src domain.SourceKind, name string, d time.Duration, err error) {
			outcome := "ok"
			if err != nil {
				outcome = string(errs.CodeOf(err))
				s.obs.Metrics.DataSourceErrors.Add(context.Background(), 1, observability.MetricAttrs(
					attribute.String(observability.AttrSource, string(src)),
					attribute.String(observability.AttrErrorCode, outcome),
				))
			}
			s.obs.Metrics.RecordSourceLatency(context.Background(), string(src), d,
				attribute.String("operation", name),
				attribute.String(observability.AttrOutcome, outcome),
			)
		},
		OnPartial: func(src domain.SourceKind) {
			s.obs.Metrics.PartialResponses.Add(context.Background(), 1, observability.MetricAttrs(
				attribute.String(observability.AttrSource, string(src)),
			))
		},
		OnElapsed: func(d time.Duration) {
			s.obs.Metrics.AggregationLatency.Record(context.Background(), d.Seconds())
		},
	}
}

// Health reports both sources' state, for the readiness probe.
func (s *Service) Health(ctx context.Context) (operational, execution datasource.Health) {
	return s.ops.Health(ctx), s.execs.Health(ctx)
}

// InvalidateResource drops every cached view of one resource. Exposed for the
// admin API and for tests; a production BFF would call it from a change
// notification.
func (s *Service) InvalidateResource(ctx context.Context, tenantID, resourceID string) {
	types := s.cfg.Get().RequestTypes()
	keys := cache.ResourceKeysForTypes(s.cache.KeyPrefix(), tenantID, resourceID, types)
	s.cache.Invalidate(ctx, keys...)
}

// targetMissesRequiredFields reports whether the chosen target cannot supply
// every field the request asked for.
func (s *Service) targetMissesRequiredFields(d router.Decision, cls classifier.Classification) bool {
	if s.catalog == nil || len(cls.RequiredFields) == 0 {
		return false
	}
	for _, field := range cls.RequiredFields {
		suppliers := s.catalog.Suppliers(field)
		if len(suppliers) == 0 {
			continue
		}
		// A field is unsatisfiable only when NO source that can supply it is in
		// the target. Testing against the most authoritative supplier alone
		// would flag every fallback answer as partial, including one whose
		// fields the fallback source can legitimately provide.
		satisfiable := false
		for _, src := range suppliers {
			if d.Target.Includes(src) {
				satisfiable = true
				break
			}
		}
		if !satisfiable {
			return true
		}
	}
	return false
}

// otherThan names the source that is not the given one, which for a two-source
// fallback is the source that failed.
func otherThan(k domain.SourceKind) domain.SourceKind {
	switch k {
	case domain.SourceOperational:
		return domain.SourceExecution
	case domain.SourceExecution:
		return domain.SourceOperational
	default:
		return domain.SourceNone
	}
}

func toRouterHealth(h datasource.Health) router.Health {
	return router.Health{Available: h.Available, Detail: h.Detail}
}

func routingDecisionFor(sources []domain.SourceKind) string {
	var ops, exec bool
	for _, s := range sources {
		switch s {
		case domain.SourceOperational:
			ops = true
		case domain.SourceExecution:
			exec = true
		}
	}
	switch {
	case ops && exec:
		return router.TargetBoth.String()
	case ops:
		return router.TargetOperational.String()
	case exec:
		return router.TargetExecution.String()
	default:
		return router.TargetNone.String()
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
