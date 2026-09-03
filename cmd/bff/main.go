// Command bff runs the TTL-aware Backend-for-Frontend.
//
// Startup order matters and is deliberate:
//
//  1. Configuration is loaded and validated before anything else. A bad
//     configuration fails here, loudly, rather than at the first request.
//  2. Telemetry comes next so that the rest of startup is observable.
//  3. Adapters are constructed but not connected. A source that is down must
//     not stop the BFF from starting: it will serve cached or fallback data,
//     and the readiness endpoint reports the source's real state.
//  4. Readiness is announced last.
//
// Shutdown reverses it, and drains connections before stopping telemetry so
// that the last requests' spans are still exported.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"gopkg.in/yaml.v3"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/api"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/application"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/cache"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/classifier"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource/execution"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource/operational"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/freshness"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/mapper"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/observability"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/policy"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/router"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/security"
)

// Build metadata, injected with -ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if err := run(); err != nil {
		// slog may not be configured yet, so this one message goes to stderr
		// directly. Everything after configuration load is structured.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath    = flag.String("config", "", "path to the configuration file; empty means defaults plus BFF_ environment overrides")
		watchInterval = flag.Duration("config-watch", 15*time.Second, "how often to re-read the configuration file; 0 disables hot reload")
		printConfig   = flag.Bool("print-config", false, "print the effective configuration and exit")
		validateOnly  = flag.Bool("validate", false, "validate configuration and exit")
		showVersion   = flag.Bool("version", false, "print build information and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("ttl-aware-bff %s (%s) built %s with %s\n", version, commit, buildDate, runtime.Version())
		return nil
	}

	provider, err := newConfigProvider(*configPath, *watchInterval)
	if err != nil {
		return err
	}
	cfg := provider.Get()

	if *validateOnly {
		fmt.Println("configuration is valid")
		return nil
	}

	log := observability.NewLogger(cfg.Observability.Log, os.Stdout)
	slog.SetDefault(log)

	if *printConfig {
		return printEffectiveConfig(cfg)
	}

	log.Info("starting",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("environment", cfg.Observability.Environment),
		slog.String("go", runtime.Version()),
		slog.Int("gomaxprocs", runtime.GOMAXPROCS(0)),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	obs, err := observability.NewProvider(ctx, cfg.Observability)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			log.Warn("telemetry shutdown reported an error", slog.String("error", err.Error()))
		}
	}()

	app, cleanup, err := build(provider, obs, log)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := api.New(api.Deps{
		Config:      provider,
		Logger:      log,
		Observer:    obs,
		Service:     app.service,
		Classifier:  app.classifier,
		Auth:        app.auth,
		RateLimiter: app.limiter,
		Build: api.BuildInfo{
			Version: version, Commit: commit, BuildDate: buildDate, GoVersion: runtime.Version(),
		},
	})

	// Configuration reloads are counted so an operator can tell a ConfigMap
	// that never propagated from one that was rejected.
	provider.OnChange(func(_, _ *config.Config) {
		obs.Metrics.ConfigReloads.Add(context.Background(), 1,
			observability.MetricAttrs(attribute.String(observability.AttrOutcome, "applied")))
	})
	go provider.Watch(ctx)

	errCh := make(chan error, 2)
	srv.Start(errCh)
	srv.MarkReady()
	log.Info("ready",
		slog.String("api", cfg.Server.HTTPAddr),
		slog.String("admin", cfg.Server.AdminAddr),
		slog.String("operational_source", cfg.Sources.Operational.Addr),
		slog.String("execution_source", cfg.Sources.Execution.BaseURL),
		slog.String("cache_backend", cfg.Cache.Backend),
	)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		log.Error("listener failed", slog.String("error", err.Error()))
		stop()
	}

	grace := cfg.Server.ShutdownGrace.D()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	log.Info("draining", slog.Duration("grace", grace))
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown did not complete cleanly", slog.String("error", err.Error()))
	}
	log.Info("stopped")
	return nil
}

// components holds what build() constructs, so main stays readable.
type components struct {
	service    *application.Service
	classifier *classifier.Classifier
	auth       *security.Authenticator
	limiter    *resilience.RateLimiter
}

// build performs dependency injection by hand. There is no container and no
// reflection: the object graph is small enough that writing it out is clearer
// than any framework, and the compiler checks it.
func build(provider *config.Provider, obs *observability.Provider, log *slog.Logger) (*components, func(), error) {
	cfg := provider.Get()

	// ---- resilience hooks, shared by both sources -------------------------
	hooks := resilience.Hooks{
		OnBreakerTransition: func(name string, from, to resilience.State) {
			obs.Metrics.BreakerTransitions.Add(context.Background(), 1, observability.MetricAttrs(
				attribute.String(observability.AttrSource, name),
				attribute.String("from", from.String()),
				attribute.String("to", to.String()),
			))
			obs.Metrics.BreakerState.Record(context.Background(), observability.BreakerStateValue(to.String()),
				observability.MetricAttrs(attribute.String(observability.AttrSource, name)))
			log.Warn("circuit breaker state change",
				slog.String(observability.AttrSource, name),
				slog.String("from", from.String()),
				slog.String("to", to.String()),
			)
		},
		OnRetry: func(name string, attempt int, delay time.Duration, err error) {
			log.Debug("retrying data source call",
				slog.String(observability.AttrSource, name),
				slog.Int("attempt", attempt),
				slog.Duration("backoff", delay),
				slog.String("error", err.Error()),
			)
		},
		OnBulkheadChange: func(name string, delta int64) {
			obs.Metrics.BulkheadInFlight.Add(context.Background(), delta,
				observability.MetricAttrs(attribute.String(observability.AttrSource, name)))
		},
	}

	opsExec := resilience.NewExecutor("OPERATIONAL", cfg.Sources.Operational.SourceCommon, hooks)
	edsExec := resilience.NewExecutor("EXECUTION", cfg.Sources.Execution.SourceCommon, hooks)

	onSchemaMismatch := func(source string) func(string) {
		return func(got string) {
			obs.Metrics.SchemaMismatch.Add(context.Background(), 1, observability.MetricAttrs(
				attribute.String(observability.AttrSource, source),
				attribute.String("declared_version", got),
			))
			log.Error("data source declared an unsupported schema version",
				slog.String(observability.AttrSource, source),
				slog.String("declared_version", got),
			)
		}
	}

	opsAdapter, err := operational.New(cfg.Sources.Operational, opsExec,
		mapper.NewOperational(cfg.Sources.Operational.AcceptedSchemaVersions, onSchemaMismatch("OPERATIONAL")))
	if err != nil {
		return nil, nil, fmt.Errorf("operational adapter: %w", err)
	}
	edsAdapter, err := execution.New(cfg.Sources.Execution, edsExec,
		mapper.NewExecution(cfg.Sources.Execution.AcceptedSchemaVersions, onSchemaMismatch("EXECUTION")))
	if err != nil {
		_ = opsAdapter.Close()
		return nil, nil, fmt.Errorf("execution adapter: %w", err)
	}

	// ---- cache ------------------------------------------------------------
	backend, err := cache.Build(cfg.Cache)
	if err != nil {
		_ = opsAdapter.Close()
		_ = edsAdapter.Close()
		return nil, nil, fmt.Errorf("cache: %w", err)
	}
	cacheMgr := cache.NewManager(backend, cfg.Cache, cache.Hooks{
		OnHit:  func(layer string) { obs.Metrics.RecordCache(context.Background(), true, layer) },
		OnMiss: func() { obs.Metrics.RecordCache(context.Background(), false, string(domain.CacheNone)) },
		OnError: func(op string, err error) {
			obs.Metrics.CacheErrors.Add(context.Background(), 1,
				observability.MetricAttrs(attribute.String("operation", op)))
			log.Warn("cache operation failed; continuing without cache",
				slog.String("operation", op), slog.String("error", err.Error()))
		},
	})

	// ---- policy, freshness, routing ---------------------------------------
	catalog := policy.NewFieldCatalog(cfg.Precedence)
	prec := policy.NewPrecedence(cfg.Precedence, func(field string, winner domain.SourceKind) {
		obs.Metrics.PrecedenceConflicts.Add(context.Background(), 1, observability.MetricAttrs(
			attribute.String("field", field),
			attribute.String("winner", string(winner)),
		))
	})

	freshMgr := freshness.NewManager(opsAdapter, cfg.Routing.Defaults.ProbeCacheTTL.D(), freshness.Hooks{
		OnProbe: func(ok bool, d time.Duration) {
			outcome := "ok"
			if !ok {
				outcome = "error"
			}
			obs.Metrics.RecordSourceLatency(context.Background(), "OPERATIONAL", d,
				attribute.String("operation", "freshness_probe"),
				attribute.String(observability.AttrOutcome, outcome),
			)
		},
		OnSkew: func(seconds float64) {
			obs.Metrics.ClockSkewDetected.Add(context.Background(), 1)
			log.Warn("data source clock disagrees with this service", slog.Float64("skew_seconds", seconds))
		},
		OnEvaluated: func(domain.FreshnessState, time.Duration) {},
	})

	rtr := router.New(provider, catalog, freshMgr, router.Hooks{
		OnDecision: func(t router.Target, rule, requestType string) {
			obs.Metrics.RoutingDecision.Add(context.Background(), 1, observability.MetricAttrs(
				attribute.String(observability.AttrRoutingTarget, t.String()),
				attribute.String(observability.AttrRoutingRule, rule),
				attribute.String(observability.AttrRequestType, requestType),
			))
		},
		OnTTL: func(hit bool, requestType string) {
			obs.Metrics.RecordTTL(context.Background(), hit,
				attribute.String(observability.AttrRequestType, requestType))
		},
		OnFallback: func(from, to domain.SourceKind, requestType string) {
			obs.Metrics.FallbackTotal.Add(context.Background(), 1, observability.MetricAttrs(
				attribute.String("from", string(from)),
				attribute.String("to", string(to)),
				attribute.String(observability.AttrRequestType, requestType),
			))
		},
		OnProbeErr: func(err error) {
			log.Debug("freshness probe failed; routing will apply the unknown-freshness policy",
				slog.String("error", err.Error()))
		},
	})

	svc := application.New(application.Deps{
		Config:      provider,
		Router:      rtr,
		Operational: opsAdapter,
		Execution:   edsAdapter,
		Precedence:  prec,
		Catalog:     catalog,
		Cache:       cacheMgr,
		Observer:    obs,
		Logger:      log,
	})

	// ---- security ---------------------------------------------------------
	auth, err := buildAuthenticator(cfg)
	if err != nil {
		_ = opsAdapter.Close()
		_ = edsAdapter.Close()
		return nil, nil, err
	}

	cleanup := func() {
		if err := opsAdapter.Close(); err != nil {
			log.Warn("closing the operational adapter reported an error", slog.String("error", err.Error()))
		}
		if err := edsAdapter.Close(); err != nil {
			log.Warn("closing the execution adapter reported an error", slog.String("error", err.Error()))
		}
		if backend != nil {
			if err := backend.Close(); err != nil {
				log.Warn("closing the cache reported an error", slog.String("error", err.Error()))
			}
		}
	}

	return &components{
		service:    svc,
		classifier: classifier.New(provider, catalog),
		auth:       auth,
		limiter:    resilience.NewRateLimiter(cfg.Security.RateLimit),
	}, cleanup, nil
}

func buildAuthenticator(cfg *config.Config) (*security.Authenticator, error) {
	if cfg.Security.AllowInsecureNoAuth {
		return security.NewAuthenticator(cfg.Security, nil), nil
	}
	if cfg.Security.JWT.JWKSURL != "" {
		return security.NewAuthenticator(cfg.Security,
			security.NewJWKSKeyProvider(cfg.Security.JWT.JWKSURL, cfg.Security.JWT.JWKSTTL.D(), nil)), nil
	}
	secret := config.SecretFromEnv(cfg.Security.JWT.HS256SecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("security: %s is not set and no JWKS URL is configured", cfg.Security.JWT.HS256SecretEnv)
	}
	kp, err := security.NewHMACKeyProvider(secret)
	if err != nil {
		return nil, err
	}
	return security.NewAuthenticator(cfg.Security, kp), nil
}

func newConfigProvider(path string, watch time.Duration) (*config.Provider, error) {
	if path == "" {
		cfg, err := config.Load("")
		if err != nil {
			return nil, err
		}
		return config.NewProvider(cfg), nil
	}
	p, err := config.NewFileProvider(path, watch, slog.Default())
	if err != nil {
		return nil, err
	}
	return p, nil
}

func printEffectiveConfig(cfg *config.Config) error {
	redacted := cfg.Redacted()
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	defer func() { _ = enc.Close() }()
	if err := enc.Encode(redacted); err != nil {
		return fmt.Errorf("printing configuration: %w", err)
	}
	return nil
}

// buildInfoFromDebug fills in build metadata when the binary was built without
// -ldflags, for example by `go run`.
func init() {
	if commit != "none" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.time":
			buildDate = s.Value
		}
	}
}
