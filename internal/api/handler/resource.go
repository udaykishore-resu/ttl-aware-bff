// Package handler contains the HTTP handlers. They are deliberately thin:
// parse and validate the request, ask the classifier what it is, check the
// caller's permission, call one application use case, write the envelope.
//
// No routing, no source knowledge, no business logic. If a handler grows past
// forty lines, something belongs in the application layer instead
// (REQ-ARCH-005).
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/middleware"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/response"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/application"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/security"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// Resource serves the /api/v1/resources endpoints.
type Resource struct {
	svc *application.Service
	cls *classifier.Classifier
	cfg *config.Provider
	out *response.Writer
	log *slog.Logger
}

// NewResource builds the handler set.
func NewResource(svc *application.Service, cls *classifier.Classifier, cfg *config.Provider, out *response.Writer, log *slog.Logger) *Resource {
	if log == nil {
		log = slog.Default()
	}
	return &Resource{svc: svc, cls: cls, cfg: cfg, out: out, log: log}
}

// method is a method expression over the application service: the route table
// names the use case, and serve() supplies the receiver. Using method
// expressions rather than closures keeps each handler a single line and makes
// the route-to-use-case mapping readable at a glance.
type method func(*application.Service, context.Context, classifier.Classification) (*application.Envelope, error)

// Get handles GET /api/v1/resources/{resourceId}.
func (h *Resource) Get(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermResourceRead, (*application.Service).GetResource)
}

// Status handles GET /api/v1/resources/{resourceId}/status.
func (h *Resource) Status(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermResourceRead, (*application.Service).GetResourceStatus)
}

// Configuration handles GET /api/v1/resources/{resourceId}/configuration.
func (h *Resource) Configuration(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermResourceRead, (*application.Service).GetResourceConfiguration)
}

// Details handles GET /api/v1/resources/{resourceId}/details.
func (h *Resource) Details(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermResourceRead, (*application.Service).GetResourceDetails)
}

// Executions handles GET /api/v1/resources/{resourceId}/executions.
func (h *Resource) Executions(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermExecutionRead, (*application.Service).ListExecutions)
}

// Execution handles GET /api/v1/resources/{resourceId}/executions/{executionId}.
func (h *Resource) Execution(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, security.PermExecutionRead, (*application.Service).GetExecution)
}

// serve is the single path every read endpoint takes.
func (h *Resource) serve(w http.ResponseWriter, r *http.Request, perm string, call method) {
	ctx := r.Context()
	cfg := h.cfg.Get()

	// Record the matched route for the access log and metrics; only the
	// dispatched request knows it.
	middleware.SetRoute(ctx, r.Pattern)

	principal := security.PrincipalFrom(ctx)
	if err := security.Authorize(principal, perm, cfg.Security.RBAC.DefaultDeny); err != nil {
		h.out.Error(w, r, err)
		return
	}

	in, err := h.parse(r, principal)
	if err != nil {
		h.out.Error(w, r, err)
		return
	}

	cls, err := h.cls.Classify(in)
	if err != nil {
		h.out.Error(w, r, err)
		return
	}

	env, err := call(h.svc, ctx, cls)
	if err != nil {
		observability.FromContext(ctx, h.log).LogAttrs(ctx, levelFor(err), "request failed",
			slog.String(observability.AttrRequestType, cls.Type),
			slog.String(observability.AttrErrorCode, string(errs.CodeOf(err))),
			slog.String("error", err.Error()),
		)
		h.out.Error(w, r, err, h.sourceStates(ctx)...)
		return
	}
	h.out.OK(w, r, response.Envelope{Data: env.Data, Meta: env.Meta})
}

// parse validates the request and turns it into classifier input.
//
// Validation is strict and happens before anything else touches the values:
// path parameters are length- and charset-checked, the page size is bounded,
// and an unknown query parameter is rejected rather than ignored, so a typo in
// a client's URL surfaces as a 400 instead of as silently different behaviour
// (REQ-API-007).
func (h *Resource) parse(r *http.Request, p *security.Principal) (classifier.Input, error) {
	resourceID := r.PathValue("resourceId")
	if err := validateID("resourceId", resourceID); err != nil {
		return classifier.Input{}, err
	}
	executionID := r.PathValue("executionId")
	if executionID != "" {
		if err := validateID("executionId", executionID); err != nil {
			return classifier.Input{}, err
		}
	}

	q := r.URL.Query()
	for key := range q {
		switch key {
		case "fields", "consistency", "limit", "cursor", "includeAudit":
		default:
			return classifier.Input{}, errs.New(errs.CodeInvalidRequest, "unrecognised query parameter").
				WithOp("handler.parse").WithDetail("parameter", key)
		}
	}

	in := classifier.Input{
		Route:                routePattern(r),
		Method:               r.Method,
		TenantID:             correlation.TenantID(r.Context()),
		ResourceID:           resourceID,
		ExecutionID:          executionID,
		RequestedConsistency: q.Get("consistency"),
		Cursor:               q.Get("cursor"),
	}

	if raw := q.Get("fields"); raw != "" {
		for _, f := range strings.Split(raw, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			in.RequestedFields = append(in.RequestedFields, f)
		}
		if len(in.RequestedFields) > 32 {
			return classifier.Input{}, errs.New(errs.CodeInvalidRequest, "too many fields requested").
				WithOp("handler.parse")
		}
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return classifier.Input{}, errs.New(errs.CodeInvalidRequest, "limit must be a positive integer").
				WithOp("handler.parse")
		}
		in.Limit = n
	}

	if raw := q.Get("includeAudit"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return classifier.Input{}, errs.New(errs.CodeInvalidRequest, "includeAudit must be true or false").
				WithOp("handler.parse")
		}
		// Asking for audit data the caller may not see is a 403, not a
		// silently narrowed response: a UI that believes it received audit
		// records when it did not is worse than an explicit refusal.
		if v && !p.Can(security.PermExecutionAudit) {
			return classifier.Input{}, errs.New(errs.CodeForbidden,
				"the caller is not permitted to read audit information").
				WithOp("handler.parse").WithDetail("permission", security.PermExecutionAudit)
		}
		in.IncludeAudit = v
	}

	if len(in.Cursor) > 512 {
		return classifier.Input{}, errs.New(errs.CodeInvalidRequest, "cursor is too long").WithOp("handler.parse")
	}
	return in, nil
}

// sourceStates reports each source's condition, so a 503 tells the caller
// which side is down without exposing addresses.
func (h *Resource) sourceStates(ctx context.Context) []response.SourceState {
	ops, exec := h.svc.Health(ctx)
	return []response.SourceState{
		{Source: domain.SourceOperational, State: ops.Detail},
		{Source: domain.SourceExecution, State: exec.Detail},
	}
}

// maxIDLength bounds path identifiers. Anything longer is either an attack or
// a bug, and both should be refused before reaching a data source.
const maxIDLength = 128

func validateID(name, v string) error {
	if v == "" {
		return errs.New(errs.CodeInvalidRequest, name+" is required").WithOp("handler.validate")
	}
	if len(v) > maxIDLength {
		return errs.New(errs.CodeInvalidRequest, name+" is too long").WithOp("handler.validate")
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return errs.New(errs.CodeInvalidRequest,
				name+" may contain only letters, digits and the characters - _ . :").
				WithOp("handler.validate")
		}
	}
	return nil
}

func routePattern(r *http.Request) string {
	p := r.Pattern
	if i := strings.IndexByte(p, ' '); i >= 0 {
		return p[i+1:]
	}
	return p
}

func levelFor(err error) slog.Level {
	if errs.HTTPStatusOf(err) >= 500 {
		return slog.LevelError
	}
	return slog.LevelWarn
}
