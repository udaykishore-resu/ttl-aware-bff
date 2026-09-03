// Package classifier turns an inbound API request into the vocabulary the
// router understands: a request type, the canonical fields the answer must
// carry, and the consistency the caller needs.
//
// Classification is deliberately separate from routing. The classifier knows
// about the API surface; the router knows about data sources. Neither knows
// about the other, which is why a new endpoint needs no routing code and a new
// data source needs no API code.
//
// Traceability: REQ-CLS-001..REQ-CLS-007.
package classifier

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// Request types. These strings are the join between the API layer, the routing
// configuration and the metrics attributes, so they are declared once here.
const (
	TypeResourceRead          = "resource_read"
	TypeResourceStatus        = "resource_status"
	TypeResourceConfiguration = "resource_configuration"
	TypeResourceDetails       = "resource_details"
	TypeExecutionStatus       = "execution_status"
	TypeExecutionHistory      = "execution_history"
)

// Consistency expresses how much staleness the caller can tolerate.
type Consistency int

const (
	// ConsistencyEventual accepts whatever is cheapest.
	ConsistencyEventual Consistency = iota
	// ConsistencyBounded accepts data within the configured TTL.
	ConsistencyBounded
	// ConsistencyStrong requires a live read of the authoritative source and
	// forbids stale-serve.
	ConsistencyStrong
)

// String renders the level for logs and metric attributes.
func (c Consistency) String() string {
	switch c {
	case ConsistencyStrong:
		return "strong"
	case ConsistencyBounded:
		return "bounded"
	default:
		return "eventual"
	}
}

// ParseConsistency maps a configuration or header token to a level.
func ParseConsistency(s string) (Consistency, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "strong":
		return ConsistencyStrong, true
	case "bounded":
		return ConsistencyBounded, true
	case "eventual", "":
		return ConsistencyEventual, true
	}
	return ConsistencyEventual, false
}

// Input is what the API layer hands the classifier.
type Input struct {
	// Route is the matched route pattern, not the raw path: classification
	// must not depend on user-supplied path values.
	Route  string
	Method string

	TenantID    string
	ResourceID  string
	ExecutionID string

	// RequestedFields is an optional caller projection (?fields=).
	RequestedFields []string
	// RequestedConsistency is an optional caller override, honoured only when
	// it is stricter than the configured default: a caller may ask for more
	// consistency than the endpoint promises, never for less (REQ-CLS-005).
	RequestedConsistency string
	// IncludeAudit reflects an explicit ?includeAudit=true, subject to RBAC.
	IncludeAudit bool
	// Limit and Cursor are history pagination inputs.
	Limit  int
	Cursor string
}

// Classification is the router's input.
type Classification struct {
	Type        string
	TenantID    string
	ResourceID  string
	ExecutionID string

	RequiredFields []string
	Consistency    Consistency
	MaxLatency     time.Duration

	IncludeAudit bool
	Limit        int
	Cursor       string
}

// Classifier maps routes to request types and validates the result against
// what the routing configuration actually knows how to route.
type Classifier struct {
	routes  map[string]string
	catalog *policy.FieldCatalog
	cfg     *config.Provider
}

// New builds a classifier. The route table is fixed at construction because it
// mirrors the OpenAPI contract; changing it is an API change, not a
// configuration change.
func New(cfg *config.Provider, catalog *policy.FieldCatalog) *Classifier {
	return &Classifier{
		cfg:     cfg,
		catalog: catalog,
		routes: map[string]string{
			"GET /api/v1/resources/{resourceId}":                          TypeResourceRead,
			"GET /api/v1/resources/{resourceId}/status":                   TypeResourceStatus,
			"GET /api/v1/resources/{resourceId}/configuration":            TypeResourceConfiguration,
			"GET /api/v1/resources/{resourceId}/details":                  TypeResourceDetails,
			"GET /api/v1/resources/{resourceId}/executions":               TypeExecutionHistory,
			"GET /api/v1/resources/{resourceId}/executions/{executionId}": TypeExecutionStatus,
		},
	}
}

// Classify produces the router's input, or a validation error.
func (c *Classifier) Classify(in Input) (Classification, error) {
	rt, ok := c.routes[in.Method+" "+in.Route]
	if !ok {
		return Classification{}, errs.New(errs.CodeInvalidRequest, "unrecognised API route").
			WithOp("classifier.route").WithDetail("route", in.Route)
	}

	cfg := c.cfg.Get()
	rule, ok := cfg.ResolveRule(in.TenantID, rt)
	if !ok {
		// A route with no routing rule is a deployment error, not a client
		// error: the service is misconfigured and should say so loudly.
		return Classification{}, errs.New(errs.CodeInternal, "no routing policy is configured for this request type").
			WithOp("classifier.rule").WithDetail("request_type", rt)
	}

	fields := in.RequestedFields
	if len(fields) == 0 {
		fields = rule.RequiredFields
	}
	if len(fields) == 0 {
		fields = policy.DefaultFieldsFor(rt)
	}
	if err := c.validateFields(fields); err != nil {
		return Classification{}, err
	}

	consistency, _ := ParseConsistency(rule.Consistency)
	if in.RequestedConsistency != "" {
		req, ok := ParseConsistency(in.RequestedConsistency)
		if !ok {
			return Classification{}, errs.New(errs.CodeInvalidRequest,
				"consistency must be one of strong, bounded, eventual").
				WithOp("classifier.consistency")
		}
		// Only tightening is allowed.
		if req > consistency {
			consistency = req
		}
	}

	limit := in.Limit
	if limit <= 0 {
		limit = cfg.Sources.Execution.HistoryPageSize
	}
	if maxItems := cfg.Sources.Execution.MaxHistoryItems; maxItems > 0 && limit > maxItems {
		limit = maxItems
	}

	return Classification{
		Type:           rt,
		TenantID:       in.TenantID,
		ResourceID:     in.ResourceID,
		ExecutionID:    in.ExecutionID,
		RequiredFields: fields,
		Consistency:    consistency,
		MaxLatency:     rule.MaxLatency.D(),
		IncludeAudit:   in.IncludeAudit,
		Limit:          limit,
		Cursor:         in.Cursor,
	}, nil
}

func (c *Classifier) validateFields(fields []string) error {
	for _, f := range fields {
		if len(c.catalog.Suppliers(f)) == 0 {
			return errs.New(errs.CodeInvalidRequest, "unknown field requested").
				WithOp("classifier.fields").
				WithDetail("field", f).
				WithDetail("known_fields", c.catalog.KnownFields())
		}
	}
	return nil
}

// Routes returns the route-to-type table, used by the API layer to register
// handlers and by tests to assert the two stay in step.
func (c *Classifier) Routes() map[string]string {
	out := make(map[string]string, len(c.routes))
	for k, v := range c.routes {
		out[k] = v
	}
	return out
}
