// Package middleware holds the inbound request pipeline.
//
// Order matters and is fixed in api.Server.routes():
//
//	recover -> correlation -> otelhttp -> access log -> body limit ->
//	rate limit -> authenticate -> tenant -> timeout -> handler
//
// Recovery is outermost so a panic anywhere still produces a correlated error
// document. Correlation comes next so every later stage, including the panic
// handler, can name the request. Rate limiting sits before authentication so
// that an unauthenticated flood costs a token-bucket check rather than a JWT
// verification.
//
// Traceability: REQ-API-005..REQ-API-010, REQ-SEC-*, REQ-MT-002.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/response"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/security"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// Middleware is the standard decorator signature.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first listed is the outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// statusRecorder captures the status code and byte count for the access log
// and for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	// wroteHeader guards against a handler that writes twice.
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer so http.ResponseController keeps
// working for flushing and deadline control.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Recover converts a panic into a 500 with a correlation id, and logs the
// stack. Without it, a panic in a handler kills the connection and the caller
// sees a transport error with nothing to quote in a support ticket.
func Recover(log *slog.Logger, w *response.Writer) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a response; it must not be logged as a crash.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "panic recovered while serving request",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				w.Error(rw, r, errs.New(errs.CodeInternal, "the request could not be completed"))
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// Correlation establishes the correlation id, request id and start time.
//
// A client-supplied correlation id is honoured so a trace can span the UI and
// the BFF, but only after sanitising: an id ends up in logs, metrics
// attributes and outbound headers, so an unvalidated one is a log-injection
// and cardinality-explosion vector (REQ-API-006, REQ-OBS-013).
func Correlation() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			cid, ok := correlation.SanitizeID(r.Header.Get(correlation.HeaderCorrelationID))
			if !ok {
				cid = correlation.NewID()
			}
			rid, ok := correlation.SanitizeID(r.Header.Get(correlation.HeaderRequestID))
			if !ok {
				rid = correlation.NewID()
			}

			ctx := correlation.WithCorrelationID(r.Context(), cid)
			ctx = correlation.WithRequestID(ctx, rid)
			ctx = correlation.WithStart(ctx, time.Now())
			ctx = WithRouteHolder(ctx)

			rw.Header().Set(correlation.HeaderCorrelationID, cid)
			rw.Header().Set(correlation.HeaderRequestID, rid)

			// Put the ids on the span so a trace can be found from a support
			// ticket that only quotes a correlation id.
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(
					attribute.String(observability.AttrCorrelationID, cid),
					attribute.String(observability.AttrRequestID, rid),
				)
			}
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// AccessLog records one structured line per request and the request metrics.
func AccessLog(log *slog.Logger, m *observability.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}

			m.ConcurrentReqs.Add(r.Context(), 1)
			defer m.ConcurrentReqs.Add(r.Context(), -1)

			next.ServeHTTP(rec, r)

			// Read after dispatch: the route is only known once the mux has
			// matched.
			route := Route(r.Context())
			d := time.Since(start)
			attrs := observability.MetricAttrs(
				attribute.String(observability.AttrHTTPRoute, route),
				attribute.String(observability.AttrHTTPMethod, r.Method),
				attribute.Int(observability.AttrHTTPStatus, rec.status),
				attribute.String(observability.AttrTenant, correlation.TenantID(r.Context())),
			)
			m.RequestTotal.Add(r.Context(), 1, attrs)
			m.RequestLatency.Record(r.Context(), d.Seconds(), attrs)

			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			observability.FromContext(r.Context(), log).LogAttrs(r.Context(), level, "request served",
				slog.String(observability.AttrHTTPMethod, r.Method),
				slog.String(observability.AttrHTTPRoute, route),
				slog.String("path", r.URL.Path),
				slog.Int(observability.AttrHTTPStatus, rec.status),
				slog.Int64("bytes", rec.written),
				slog.Float64("duration_ms", float64(d.Microseconds())/1000),
				slog.String("degraded", rec.Header().Get("X-BFF-Degraded")),
				slog.String("freshness", rec.Header().Get("X-BFF-Freshness")),
			)
		})
	}
}

// BodyLimit caps the request body. The BFF's API is read-only, so any body at
// all is unexpected; the limit exists so that a client streaming megabytes at
// a GET cannot occupy a connection indefinitely (REQ-SEC-011).
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				http.Error(rw, `{"error":{"code":"REQUEST_TOO_LARGE","status":413}}`, http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(rw, r.Body, maxBytes)
			next.ServeHTTP(rw, r)
		})
	}
}

// RateLimit admits requests per tenant. Before authentication the tenant is
// not yet known, so the limiter falls back to the client address; after
// authentication the per-tenant limiter in the service layer applies.
func RateLimit(limiter *resilience.RateLimiter, m *observability.Metrics, w *response.Writer) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			key := correlation.TenantID(r.Context())
			if key == "" {
				key = clientIP(r)
			}
			if !limiter.Allow(key) {
				m.RateLimited.Add(r.Context(), 1)
				rw.Header().Set("Retry-After", "1")
				w.Error(rw, r, errs.ErrRateLimited)
				return
			}
			next.ServeHTTP(rw, r)
		})
	}
}

// Authenticate verifies credentials and establishes the tenant context.
func Authenticate(auth *security.Authenticator, w *response.Writer, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			p, err := auth.Authenticate(r.Context(), r.Header)
			if err != nil {
				logAuthFailure(r, log, err)
				w.Error(rw, r, err)
				return
			}
			tenant, err := security.ResolveTenant(p, r.Header.Get(correlation.HeaderTenantID))
			if err != nil {
				logAuthFailure(r, log, err)
				w.Error(rw, r, err)
				return
			}

			ctx := correlation.WithTenantID(r.Context(), tenant)
			ctx = correlation.WithPrincipal(ctx, p.Subject)
			ctx = correlation.WithRoles(ctx, p.Roles)
			ctx = security.WithPrincipal(ctx, p)

			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(attribute.String(observability.AttrTenant, tenant))
			}
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// logAuthFailure records the real reason at warn level while the client only
// ever receives the generic message.
func logAuthFailure(r *http.Request, log *slog.Logger, err error) {
	observability.FromContext(r.Context(), log).LogAttrs(r.Context(), slog.LevelWarn,
		"authentication or tenant resolution failed",
		slog.String(observability.AttrErrorCode, string(errs.CodeOf(err))),
		slog.String("error", err.Error()),
		slog.String("path", r.URL.Path),
		slog.String("client_ip", clientIP(r)),
	)
}

// Timeout bounds the whole request. Per-source timeouts are carved out of what
// remains of this budget, so a slow source can never outlive its request.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// SecurityHeaders sets the response headers appropriate to a JSON API.
func SecurityHeaders(cfg config.ServerConfig) Middleware {
	_ = cfg
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			h := rw.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			// The API returns no HTML, so the strictest possible policy is
			// also the correct one.
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(rw, r)
		})
	}
}

// routeHolder carries the matched route pattern back out to the access log.
//
// It is needed because http.ServeMux sets Request.Pattern on the request it
// passes to the handler, which is a clone: the outer middleware never sees it.
// The handler therefore records the pattern here, and the access log reads it
// after the handler returns. Using the pattern rather than the raw path is what
// keeps metric and span cardinality bounded, since the path contains resource
// ids (REQ-OBS-013).
type routeHolder struct{ pattern string }

type routeKey struct{}

// WithRouteHolder installs the holder. Called by Correlation, which is the
// first middleware every request passes through.
func WithRouteHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, routeKey{}, &routeHolder{})
}

// SetRoute records the matched route pattern. Handlers call it once they have
// been dispatched and Request.Pattern is populated.
func SetRoute(ctx context.Context, pattern string) {
	h, ok := ctx.Value(routeKey{}).(*routeHolder)
	if !ok {
		return
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	h.pattern = pattern
}

// Route returns the recorded route pattern, or "unmatched".
func Route(ctx context.Context) string {
	h, ok := ctx.Value(routeKey{}).(*routeHolder)
	if !ok || h.pattern == "" {
		return "unmatched"
	}
	return h.pattern
}

func clientIP(r *http.Request) string {
	// X-Forwarded-For is only meaningful behind a trusted proxy; the ALB in
	// front of this service sets it, and the ingress strips any client-sent
	// value. The left-most entry is the original client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		return host[:i]
	}
	return host
}
