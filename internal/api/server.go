// Package api wires the HTTP surface: the public v1 API and the separate
// admin listener that carries health probes and metrics.
//
// The two listeners are separate on purpose. Probes and metrics must stay
// reachable when the public port is saturated or rate-limited, and they must
// never be exposed through the ingress (REQ-OBS-012, REQ-SEC-012).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/handler"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/middleware"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/api/response"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/application"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/security"
)

// Server owns both listeners and their lifecycle.
type Server struct {
	cfg *config.Provider
	log *slog.Logger
	obs *observability.Provider
	svc *application.Service
	out *response.Writer

	public *http.Server
	admin  *http.Server

	// ready flips to true once startup has finished and flips back to false
	// when shutdown begins, so a draining pod fails readiness before it stops
	// accepting connections.
	ready atomic.Bool
	// live stays true unless the process has decided it cannot recover.
	live atomic.Bool

	buildInfo BuildInfo
}

// BuildInfo is reported by the admin surface.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// Deps groups what the server needs.
type Deps struct {
	Config      *config.Provider
	Logger      *slog.Logger
	Observer    *observability.Provider
	Service     *application.Service
	Classifier  *classifier.Classifier
	Auth        *security.Authenticator
	RateLimiter *resilience.RateLimiter
	Build       BuildInfo
}

// New builds the server and its routes.
func New(d Deps) *Server {
	cfg := d.Config.Get()
	out := response.NewWriter(d.Logger)

	s := &Server{
		cfg:       d.Config,
		log:       d.Logger,
		obs:       d.Observer,
		svc:       d.Service,
		out:       out,
		buildInfo: d.Build,
	}
	s.live.Store(true)

	s.public = &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           s.publicRoutes(d),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		ReadHeaderTimeout: cfg.Server.HeaderTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
		ErrorLog:          slog.NewLogLogger(d.Logger.Handler(), slog.LevelWarn),
	}
	s.admin = &http.Server{
		Addr:              cfg.Server.AdminAddr,
		Handler:           s.adminRoutes(),
		ReadHeaderTimeout: 2 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.Logger.Handler(), slog.LevelWarn),
	}
	return s
}

// publicRoutes builds the v1 API. The route patterns here and the classifier's
// route table are the same strings; a test asserts they stay in step, because
// a route registered here but unknown to the classifier would 500 at runtime.
func (s *Server) publicRoutes(d Deps) http.Handler {
	cfg := d.Config.Get()
	h := handler.NewResource(d.Service, d.Classifier, d.Config, s.out, d.Logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/resources/{resourceId}", h.Get)
	mux.HandleFunc("GET /api/v1/resources/{resourceId}/status", h.Status)
	mux.HandleFunc("GET /api/v1/resources/{resourceId}/configuration", h.Configuration)
	mux.HandleFunc("GET /api/v1/resources/{resourceId}/details", h.Details)
	mux.HandleFunc("GET /api/v1/resources/{resourceId}/executions", h.Executions)
	mux.HandleFunc("GET /api/v1/resources/{resourceId}/executions/{executionId}", h.Execution)

	// Anything else is a 404 in the service's own error format, not Go's
	// plain-text default: the UI parses one error shape, always.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.out.Error(w, r, notFoundErr())
	})

	// otelhttp names spans by route pattern, which keeps trace cardinality
	// bounded the same way the metric attributes do.
	instrumented := otelhttp.NewHandler(mux, "bff.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return r.Method + " unmatched"
		}),
	)

	return middleware.Chain(instrumented,
		middleware.Recover(d.Logger, s.out),
		middleware.Correlation(),
		middleware.SecurityHeaders(cfg.Server),
		middleware.AccessLog(d.Logger, d.Observer.Metrics),
		middleware.BodyLimit(cfg.Server.MaxBodyBytes),
		middleware.RateLimit(d.RateLimiter, d.Observer.Metrics, s.out),
		middleware.Authenticate(d.Auth, s.out, d.Logger),
		middleware.Timeout(cfg.Server.RequestTimeout.D()),
	)
}

// adminRoutes builds the operational surface: probes, metrics and build info.
// It is deliberately unauthenticated and deliberately not routed through the
// ingress; the NetworkPolicy in deploy/k8s restricts it to the cluster.
func (s *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()

	// Liveness answers "should the kubelet restart this process?" It must not
	// depend on any external system: a source outage is not a reason to
	// restart the BFF, and a liveness probe that checks dependencies turns a
	// dependency blip into a cluster-wide restart storm.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		if !s.live.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "dead"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})

	// Readiness answers "should traffic be sent here?" It reports the sources'
	// state but does NOT fail on a source outage either: with both sources
	// down the BFF can still serve stale cached data, and removing every pod
	// from the load balancer would turn a degraded service into no service.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
			return
		}
		ops, exec := s.svc.Health(r.Context())
		body := map[string]any{
			"status": "ready",
			"sources": map[string]any{
				"operational": map[string]any{"available": ops.Available, "state": ops.Detail},
				"execution":   map[string]any{"available": exec.Available, "state": exec.Detail},
			},
		}
		if !ops.Available && !exec.Available {
			body["status"] = "degraded"
		}
		writeJSON(w, http.StatusOK, body)
	})

	// Startup gates the other probes while the process warms up.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		state := "ok"
		if !s.ready.Load() {
			status, state = http.StatusServiceUnavailable, "starting"
		}
		writeJSON(w, status, map[string]any{"status": state, "build": s.buildInfo})
	})

	mux.Handle("GET /metrics", observability.PrometheusHandler(s.obs.PromReader))

	mux.HandleFunc("GET /buildinfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.buildInfo)
	})

	// The effective routing policy, redacted. Being able to ask a running pod
	// "what TTL are you actually using for this request type?" removes most of
	// the guesswork from a routing incident.
	mux.HandleFunc("GET /config/routing", func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Get()
		tenant := r.URL.Query().Get("tenant")
		out := map[string]any{}
		for name := range cfg.Routing.RequestTypes {
			rule, _ := cfg.ResolveRule(tenant, name)
			out[name] = map[string]any{
				"preferred_source": rule.PreferredSource,
				"ttl":              rule.TTL.String(),
				"cache_ttl":        rule.CacheTTL.String(),
				"fallback":         rule.Fallback,
				"allow_stale":      rule.AllowStale,
				"max_stale":        rule.MaxStale.String(),
				"consistency":      rule.Consistency,
			}
		}
		reloads, failures := s.cfg.Stats()
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":          tenant,
			"request_types":   out,
			"defaults":        cfg.ResolveDefaults(tenant),
			"reload_count":    reloads,
			"reload_failures": failures,
		})
	})

	return mux
}

// Start begins serving. It returns once both listeners are up; serving
// continues in background goroutines until Shutdown.
func (s *Server) Start(errCh chan<- error) {
	go func() {
		s.log.Info("public API listening", slog.String("addr", s.public.Addr))
		if err := s.public.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		s.log.Info("admin listening", slog.String("addr", s.admin.Addr))
		if err := s.admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
}

// MarkReady flips readiness on once startup work has finished.
func (s *Server) MarkReady() { s.ready.Store(true) }

// Shutdown drains connections.
//
// Readiness is turned off first and the caller is expected to have waited for
// the load balancer to notice (the preStop hook in the Kubernetes manifests
// does exactly that) before the grace period starts. Draining without that
// wait produces connection resets during every rolling deploy.
func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)

	var errs []error
	if err := s.public.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.admin.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// PublicHandler exposes the public handler for tests that drive the API with
// httptest rather than over a socket.
func (s *Server) PublicHandler() http.Handler { return s.public.Handler }

// AdminHandler exposes the admin handler for tests.
func (s *Server) AdminHandler() http.Handler { return s.admin.Handler }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
