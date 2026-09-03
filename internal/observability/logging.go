// Package observability wires tracing, metrics and structured logging.
//
// Everything the service emits goes through here so that attribute names,
// redaction rules and cardinality limits are decided in one place rather than
// at every call site.
//
// Traceability: REQ-OBS-001..REQ-OBS-014, REQ-SEC-009.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
)

// Attribute keys. Every log record, metric and span uses these exact strings.
const (
	AttrTenant        = "tenant_id"
	AttrCorrelationID = "correlation_id"
	AttrRequestID     = "request_id"
	AttrPrincipal     = "principal"
	AttrRequestType   = "request_type"
	AttrRoutingRule   = "routing_rule"
	AttrRoutingTarget = "routing_decision"
	AttrSource        = "source"
	AttrOutcome       = "outcome"
	AttrHTTPStatus    = "http_status"
	AttrHTTPMethod    = "http_method"
	AttrHTTPRoute     = "http_route"
	AttrDegraded      = "degraded"
	AttrPartial       = "partial"
	AttrCacheLayer    = "cache_layer"
	AttrErrorCode     = "error_code"
	AttrFreshness     = "freshness_state"
	AttrTraceID       = "trace_id"
	AttrSpanID        = "span_id"
	AttrBreakerState  = "breaker_state"
)

const redacted = "[REDACTED]"

// NewLogger builds the service logger from configuration.
func NewLogger(cfg config.LogConfig, w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	redact := make(map[string]struct{}, len(cfg.RedactKeys))
	for _, k := range cfg.RedactKeys {
		redact[strings.ToLower(k)] = struct{}{}
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if _, ok := redact[strings.ToLower(a.Key)]; ok {
				return slog.String(a.Key, redacted)
			}
			return a
		},
	}

	var h slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// FromContext returns a logger pre-populated with the request's identity and
// the active trace/span ids, so that a log line can always be joined to a
// trace and to a tenant (REQ-OBS-006).
//
// It never returns nil: an absent logger degrades to the default one.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	attrs := make([]any, 0, 10)
	if v := correlation.CorrelationID(ctx); v != "" {
		attrs = append(attrs, slog.String(AttrCorrelationID, v))
	}
	if v := correlation.RequestID(ctx); v != "" {
		attrs = append(attrs, slog.String(AttrRequestID, v))
	}
	if v := correlation.TenantID(ctx); v != "" {
		attrs = append(attrs, slog.String(AttrTenant, v))
	}
	if v := correlation.Principal(ctx); v != "" {
		attrs = append(attrs, slog.String(AttrPrincipal, v))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, slog.String(AttrTraceID, sc.TraceID().String()))
		attrs = append(attrs, slog.String(AttrSpanID, sc.SpanID().String()))
	}
	if len(attrs) == 0 {
		return base
	}
	return base.With(attrs...)
}
