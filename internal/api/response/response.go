// Package response owns the wire format: the success envelope, the error
// document, and the rules about which HTTP status each outcome gets.
//
// Nothing else in the service writes to an http.ResponseWriter, so the shape
// of a BFF response is decided in exactly one file.
//
// Traceability: REQ-API-003, REQ-API-004, REQ-ERR-001..REQ-ERR-012.
package response

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// Envelope is the success document. Every 2xx body has exactly this shape,
// whichever source or sources answered, which is what lets the UI treat the
// BFF as one API rather than as two proxied systems.
type Envelope struct {
	Data any                 `json:"data"`
	Meta domain.ResponseMeta `json:"meta"`
}

// ErrorDocument is the failure document, shaped after RFC 7807 with the
// machine-readable code the UI actually branches on.
type ErrorDocument struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the failure detail.
type ErrorBody struct {
	Code          errs.Code     `json:"code"`
	Type          string        `json:"type"`
	Title         string        `json:"title"`
	Status        int           `json:"status"`
	Detail        string        `json:"detail,omitempty"`
	CorrelationID string        `json:"correlationId,omitempty"`
	Retryable     bool          `json:"retryable"`
	Sources       []SourceState `json:"sources,omitempty"`
}

// SourceState reports one source's condition at the time of the failure. It is
// safe to expose: it names the *role* of a source, never its address.
type SourceState struct {
	Source domain.SourceKind `json:"source"`
	State  string            `json:"state"`
}

// errorTypeBase is the documentation namespace for error types.
const errorTypeBase = "https://errors.bff.internal/"

var errorTitles = map[errs.Code]string{
	errs.CodeInvalidRequest:         "Invalid request",
	errs.CodeUnauthenticated:        "Authentication required",
	errs.CodeForbidden:              "Access denied",
	errs.CodeTenantMismatch:         "Tenant mismatch",
	errs.CodeNotFound:               "Not found",
	errs.CodeRateLimited:            "Rate limit exceeded",
	errs.CodeRequestTooLarge:        "Request too large",
	errs.CodeUpstreamTimeout:        "Upstream data source timed out",
	errs.CodeUpstreamUnavailable:    "Upstream data source unavailable",
	errs.CodeUpstreamInvalidPayload: "Upstream data source returned an invalid response",
	errs.CodeSchemaVersionMismatch:  "Upstream schema version is not supported",
	errs.CodeNoSourceAvailable:      "No data source can satisfy this request",
	errs.CodeInternal:               "Internal error",
}

// typeSlug turns a code into its documentation URL fragment.
func typeSlug(c errs.Code) string {
	out := make([]byte, 0, len(c))
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch {
		case ch == '_':
			out = append(out, '-')
		case ch >= 'A' && ch <= 'Z':
			out = append(out, ch-'A'+'a')
		default:
			out = append(out, ch)
		}
	}
	return string(out)
}

// Writer encodes responses and records the status for the middleware chain.
type Writer struct {
	log *slog.Logger
}

// NewWriter builds a response writer.
func NewWriter(log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	return &Writer{log: log}
}

// OK writes a success envelope, choosing the status from the metadata.
//
// A partial response gets 206 rather than 200: the UI can render it, but a
// caching layer or a monitoring dashboard can tell the difference without
// parsing the body. A degraded-but-complete response stays 200 with
// meta.degraded set, because the data is whole, only older than intended
// (REQ-ERR-009).
func (w *Writer) OK(rw http.ResponseWriter, r *http.Request, env Envelope) {
	status := http.StatusOK
	if env.Meta.Partial {
		status = http.StatusPartialContent
	}
	if env.Meta.CorrelationID == "" {
		env.Meta.CorrelationID = correlation.CorrelationID(r.Context())
	}
	if env.Meta.Sources == nil {
		env.Meta.Sources = []domain.SourceKind{}
	}
	if env.Meta.Degraded {
		// Advertise degradation in a header too, so an operator watching an
		// access log can see it without a body parser.
		rw.Header().Set("X-BFF-Degraded", "true")
	}
	rw.Header().Set("X-BFF-Freshness", string(env.Meta.Freshness.State))
	rw.Header().Set("X-BFF-Source", env.Meta.RoutingDecision)
	w.write(rw, r, status, env)
}

// Error writes the error document for any error.
func (w *Writer) Error(rw http.ResponseWriter, r *http.Request, err error, sources ...SourceState) {
	code := errs.CodeOf(err)
	status := errs.HTTPStatusOf(err)
	title, ok := errorTitles[code]
	if !ok {
		title = "Error"
	}

	body := ErrorBody{
		Code:          code,
		Type:          errorTypeBase + typeSlug(code),
		Title:         title,
		Status:        status,
		CorrelationID: correlation.CorrelationID(r.Context()),
		Retryable:     errs.IsRetryable(err),
		Sources:       sources,
	}
	// Only the taxonomy's own message reaches the client. A wrapped cause may
	// name an internal host or a source schema, so it is logged, never sent
	// (REQ-SEC-009).
	if e, ok := errs.As(err); ok {
		body.Detail = e.Message
	} else if status < 500 {
		body.Detail = "the request could not be processed"
	}

	w.write(rw, r, status, ErrorDocument{Error: body})
}

func (w *Writer) write(rw http.ResponseWriter, r *http.Request, status int, payload any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	if cid := correlation.CorrelationID(r.Context()); cid != "" {
		rw.Header().Set(correlation.HeaderCorrelationID, cid)
	}
	rw.WriteHeader(status)

	enc := json.NewEncoder(rw)
	// HTML escaping off: this is an API, and escaping mangles URLs and
	// identifiers that legitimately contain &, < or >.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// The status line is already on the wire, so the only thing left is to
		// record the failure. Writing a second body would corrupt the response.
		w.log.LogAttrs(r.Context(), slog.LevelError, "failed to encode response body",
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path),
		)
	}
}

// NoContent writes an empty 204.
func (w *Writer) NoContent(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusNoContent)
}
