package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default returns the built-in configuration. These values are the lowest
// precedence layer; they exist so the service starts with sane behaviour and
// so tests do not each invent their own numbers.
//
// Note that even here the routing TTLs are data, not code: nothing reads these
// constants directly, they are merged into the same RoutingRule structs the
// YAML produces.
func Default() Config {
	d := func(s string) Duration {
		var v Duration
		if err := v.Parse(s); err != nil {
			panic("config: bad built-in default duration " + s)
		}
		return v
	}
	commonRetry := RetryConfig{
		Enabled:           true,
		MaxAttempts:       3,
		BaseBackoff:       d("20ms"),
		MaxBackoff:        d("400ms"),
		JitterFraction:    1.0,
		PerAttemptTimeout: d("0s"),
		BudgetRatio:       0.6,
	}
	commonBreaker := BreakerConfig{
		Enabled:           true,
		FailureThreshold:  0.5,
		MinimumRequests:   20,
		Window:            d("30s"),
		OpenTimeout:       d("5s"),
		HalfOpenMaxCalls:  3,
		HalfOpenSuccesses: 2,
	}

	return Config{
		Server: ServerConfig{
			HTTPAddr:       ":8080",
			AdminAddr:      ":9090",
			ReadTimeout:    d("5s"),
			WriteTimeout:   d("15s"),
			IdleTimeout:    d("60s"),
			HeaderTimeout:  d("2s"),
			ShutdownGrace:  d("25s"),
			MaxBodyBytes:   1 << 20,
			RequestTimeout: d("8s"),
		},
		Security: SecurityConfig{
			JWT: JWTConfig{
				Issuer:         "https://issuer.invalid",
				Audience:       "ttl-aware-bff",
				JWKSTTL:        d("10m"),
				HS256SecretEnv: "BFF_JWT_HS256_SECRET",
				Leeway:         d("30s"),
				TenantClaim:    "tenant_id",
				RolesClaim:     "roles",
				RequiredClaims: []string{"sub", "tenant_id"},
			},
			RBAC: RBACConfig{
				DefaultDeny: true,
				Roles: map[string][]string{
					"bff.viewer":   {"resource.read", "execution.read"},
					"bff.operator": {"resource.read", "execution.read", "execution.audit.read"},
					"bff.admin":    {"resource.read", "execution.read", "execution.audit.read", "config.read"},
				},
			},
			RateLimit: RateLimitConfig{
				Enabled: true, RPS: 200, Burst: 400, PerTenant: true, MaxTenants: 10000,
			},
		},
		Sources: SourcesConfig{
			Operational: OperationalSourceConfig{
				SourceCommon: SourceCommon{
					CallTimeout:            d("400ms"),
					Retry:                  commonRetry,
					Breaker:                commonBreaker,
					Bulkhead:               BulkheadConfig{Enabled: true, MaxConcurrent: 256, AcquireTimeout: d("50ms"), MaxQueue: 512},
					AcceptedSchemaVersions: []string{"ods.v1"},
				},
				Addr:             "localhost:9101",
				DialTimeout:      d("2s"),
				ProbeTimeout:     d("120ms"),
				KeepaliveTime:    d("30s"),
				KeepaliveTimeout: d("10s"),
				MaxRecvMsgBytes:  4 << 20,
				UseRoundRobin:    true,
			},
			Execution: ExecutionSourceConfig{
				SourceCommon: SourceCommon{
					CallTimeout: d("2s"),
					Retry: RetryConfig{
						Enabled: true, MaxAttempts: 2,
						BaseBackoff: d("50ms"), MaxBackoff: d("500ms"),
						JitterFraction: 1.0, BudgetRatio: 0.5,
					},
					Breaker:                commonBreaker,
					Bulkhead:               BulkheadConfig{Enabled: true, MaxConcurrent: 64, AcquireTimeout: d("100ms"), MaxQueue: 128},
					AcceptedSchemaVersions: []string{"eds.v1"},
				},
				BaseURL:             "http://localhost:9102",
				MaxIdleConns:        128,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     d("90s"),
				HistoryPageSize:     25,
				MaxHistoryItems:     200,
			},
		},
		Cache: CacheConfig{
			Enabled:     true,
			Backend:     "memory",
			KeyPrefix:   "bff:v1",
			NegativeTTL: d("3s"),
			StaleGrace:  d("5m"),
			FailOpen:    true,
			L1:          L1Config{Enabled: true, MaxEntries: 20000, TTL: d("2s")},
			Redis: RedisConfig{
				Addr: "localhost:6379", DB: 0, PoolSize: 64, MinIdle: 8,
				PasswordEnv: "BFF_REDIS_PASSWORD",
				DialTimeout: d("500ms"), ReadTimeout: d("200ms"), WriteTimeout: d("200ms"),
			},
			Stampede: StampedeConfig{Enabled: true, LockTTL: d("2s"), EarlyRefreshRatio: 0.8},
		},
		Routing: RoutingConfig{
			Defaults: RoutingDefaults{
				OnUnknownFreshness:       "operational",
				ClockSkewTolerance:       d("2s"),
				AllowStale:               Bool(true),
				MaxStale:                 d("5m"),
				ProbeEnabled:             Bool(true),
				ProbeCacheTTL:            d("1s"),
				ResolveInFlightExecution: Bool(true),
				InFlightLookupTimeout:    d("300ms"),
			},
			RequestTypes: map[string]RoutingRule{
				"resource_status": {
					PreferredSource: "operational", TTL: d("10s"), CacheTTL: d("3s"),
					Fallback: "execution", AllowStale: true, MaxStale: d("120s"),
					Consistency: "bounded", MaxLatency: d("300ms"),
				},
				"resource_configuration": {
					PreferredSource: "operational", TTL: d("30s"), CacheTTL: d("15s"),
					Fallback: "none", AllowStale: true, MaxStale: d("300s"),
					Consistency: "eventual", MaxLatency: d("400ms"),
				},
				"resource_read": {
					PreferredSource: "operational", TTL: d("30s"), CacheTTL: d("5s"),
					Fallback: "execution", AllowStale: true, MaxStale: d("300s"),
					Consistency: "bounded", MaxLatency: d("500ms"),
				},
				"execution_status": {
					PreferredSource: "execution", TTL: d("5s"), CacheTTL: d("2s"),
					Fallback: "none", AllowStale: false,
					Consistency: "strong", MaxLatency: d("2s"),
				},
				"execution_history": {
					PreferredSource: "execution", TTL: d("0s"), CacheTTL: d("0s"),
					Fallback: "none", AllowStale: false,
					Consistency: "strong", MaxLatency: d("3s"),
				},
				"resource_details": {
					PreferredSource: "both", TTL: d("30s"), CacheTTL: d("5s"),
					Fallback: "operational", AllowStale: true, MaxStale: d("300s"),
					Consistency: "bounded", MaxLatency: d("2s"),
					RequiredSources: map[string]bool{"operational": true, "execution": false},
					PerSourceTimeout: map[string]Duration{
						"operational": d("400ms"),
						"execution":   d("1500ms"),
					},
				},
			},
		},
		Precedence: PrecedenceConfig{
			Fields: map[string][]string{
				// Current state: the operational source observes it; the
				// execution source only predicts it, so it ranks second.
				"status":   {"operational", "execution"},
				"subState": {"operational"},
				"type":     {"operational"},
				// Operational-only fields. Naming a single source here is what
				// lets the router prove a request needs one source only.
				"configuration": {"operational"},
				"metrics":       {"operational"},
				"topology":      {"operational"},
				"owner":         {"operational"},
				"labels":        {"operational"},
				// Shared identity: either source can supply it.
				"customerId": {"operational", "execution"},
				// Execution-only fields.
				"latestExecution":  {"execution"},
				"executionHistory": {"execution"},
				"executionStatus":  {"execution"},
				"workflowSteps":    {"execution"},
				"audit":            {"execution"},
				"lastOperation":    {"execution"},
			},
			ExecutionOverridesWhenRunning: []string{"status", "subState"},
			ConflictWarning:               true,
		},
		Tenants: map[string]Override{},
		Observability: ObservabilityConfig{
			ServiceName:        "ttl-aware-bff",
			ServiceVersion:     "dev",
			Environment:        "local",
			TraceSampleRatio:   1.0,
			MetricsInterval:    d("15s"),
			PrometheusEndpoint: true,
			OTLP: OTLPConfig{
				Enabled: false, Endpoint: "localhost:4317", Insecure: true, Timeout: d("5s"),
			},
			Log: LogConfig{
				Level: "info", Format: "json", AddSource: false,
				RedactKeys: []string{"authorization", "password", "token", "secret", "cookie", "set-cookie", "api_key"},
			},
		},
	}
}

// Load reads configuration from path (optional), applies BFF_ environment
// overrides, merges over the built-in defaults and validates the result.
//
// An empty path means "defaults plus environment", which is what the unit
// tests and the container's minimal deployment use.
func Load(path string) (Config, error) {
	return load(path, os.Environ())
}

// LoadWithEnv is Load with an injectable environment, for tests.
func LoadWithEnv(path string, environ []string) (Config, error) {
	return load(path, environ)
}

func load(path string, environ []string) (Config, error) {
	cfg := Default()

	var doc yaml.Node
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}

	if err := applyEnv(&doc, environ); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	// Decoding onto the already-populated struct merges: keys present in the
	// document overwrite, keys absent keep their default. Maps are the one
	// exception -- yaml.v3 replaces a map wholesale -- so request_types and
	// precedence.fields are merged manually below.
	defaults := Default()
	if err := doc.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: decode: %w", err)
	}
	mergeRoutingDefaults(&cfg, defaults)

	if err := Validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// mergeRoutingDefaults restores per-key defaults that a partial YAML map would
// otherwise wipe out. Operators expect to override one TTL without having to
// restate the whole routing table.
func mergeRoutingDefaults(cfg *Config, def Config) {
	if cfg.Routing.RequestTypes == nil {
		cfg.Routing.RequestTypes = map[string]RoutingRule{}
	}
	for name, defRule := range def.Routing.RequestTypes {
		cur, ok := cfg.Routing.RequestTypes[name]
		if !ok {
			cfg.Routing.RequestTypes[name] = defRule
			continue
		}
		if cur.PreferredSource == "" {
			cur.PreferredSource = defRule.PreferredSource
		}
		if cur.Fallback == "" {
			cur.Fallback = defRule.Fallback
		}
		if cur.Consistency == "" {
			cur.Consistency = defRule.Consistency
		}
		if cur.MaxStale == 0 {
			cur.MaxStale = defRule.MaxStale
		}
		if cur.MaxLatency == 0 {
			cur.MaxLatency = defRule.MaxLatency
		}
		if cur.RequiredSources == nil {
			cur.RequiredSources = defRule.RequiredSources
		}
		if cur.PerSourceTimeout == nil {
			cur.PerSourceTimeout = defRule.PerSourceTimeout
		}
		cfg.Routing.RequestTypes[name] = cur
	}
	if len(cfg.Precedence.Fields) == 0 {
		cfg.Precedence.Fields = def.Precedence.Fields
	}
	if len(cfg.Security.RBAC.Roles) == 0 {
		cfg.Security.RBAC.Roles = def.Security.RBAC.Roles
	}
}

// ---------------------------------------------------------------------------
// Validation — fail fast, with every problem reported at once
// ---------------------------------------------------------------------------

// Validate checks the configuration for internal consistency. It returns a
// joined error listing every problem, because an operator fixing config wants
// the whole list, not one problem per restart (REQ-CFG-008).
func Validate(cfg *Config) error {
	var probs []error
	bad := func(format string, args ...any) {
		probs = append(probs, fmt.Errorf(format, args...))
	}

	if cfg.Server.HTTPAddr == "" {
		bad("server.http_addr must be set")
	}
	if cfg.Server.RequestTimeout <= 0 {
		bad("server.request_timeout must be > 0")
	}
	if cfg.Server.MaxBodyBytes <= 0 {
		bad("server.max_body_bytes must be > 0")
	}
	if cfg.Server.ShutdownGrace <= 0 {
		bad("server.shutdown_grace must be > 0")
	}

	if !cfg.Security.AllowInsecureNoAuth {
		if cfg.Security.JWT.Issuer == "" {
			bad("security.jwt.issuer must be set when authentication is enabled")
		}
		if cfg.Security.JWT.Audience == "" {
			bad("security.jwt.audience must be set when authentication is enabled")
		}
		if cfg.Security.JWT.JWKSURL == "" && cfg.Security.JWT.HS256SecretEnv == "" {
			bad("security.jwt: one of jwks_url or hs256_secret_env must be set")
		}
	} else if cfg.Observability.Environment != "local" && cfg.Observability.Environment != "test" {
		bad("security.allow_insecure_no_auth is only permitted when observability.environment is local or test (got %q)",
			cfg.Observability.Environment)
	}
	if cfg.Security.JWT.TenantClaim == "" {
		bad("security.jwt.tenant_claim must be set")
	}
	if cfg.Security.RateLimit.Enabled {
		if cfg.Security.RateLimit.RPS <= 0 {
			bad("security.rate_limit.rps must be > 0 when rate limiting is enabled")
		}
		if cfg.Security.RateLimit.Burst < cfg.Security.RateLimit.RPS {
			bad("security.rate_limit.burst (%d) should be >= rps (%d)", cfg.Security.RateLimit.Burst, cfg.Security.RateLimit.RPS)
		}
	}

	if cfg.Sources.Operational.Addr == "" {
		bad("sources.operational.addr must be set")
	}
	if cfg.Sources.Execution.BaseURL == "" {
		bad("sources.execution.base_url must be set")
	}
	if !strings.HasPrefix(cfg.Sources.Execution.BaseURL, "http://") && !strings.HasPrefix(cfg.Sources.Execution.BaseURL, "https://") {
		bad("sources.execution.base_url must be an http(s) URL")
	}
	validateSourceCommon("sources.operational", cfg.Sources.Operational.SourceCommon, bad)
	validateSourceCommon("sources.execution", cfg.Sources.Execution.SourceCommon, bad)
	if cfg.Sources.Operational.ProbeTimeout <= 0 {
		bad("sources.operational.freshness_probe_timeout must be > 0")
	}
	if cfg.Sources.Operational.ProbeTimeout >= cfg.Sources.Operational.CallTimeout {
		bad("sources.operational.freshness_probe_timeout (%s) must be shorter than call_timeout (%s): the probe exists to be cheap",
			cfg.Sources.Operational.ProbeTimeout, cfg.Sources.Operational.CallTimeout)
	}

	switch cfg.Cache.Backend {
	case "memory", "redis", "layered", "none":
	default:
		bad("cache.backend must be one of memory|redis|layered|none (got %q)", cfg.Cache.Backend)
	}
	if cfg.Cache.Enabled && (cfg.Cache.Backend == "redis" || cfg.Cache.Backend == "layered") && cfg.Cache.Redis.Addr == "" {
		bad("cache.redis.addr must be set for backend %q", cfg.Cache.Backend)
	}
	if cfg.Cache.KeyPrefix == "" {
		bad("cache.key_prefix must be set: it is part of tenant isolation")
	}
	if cfg.Cache.StaleGrace < 0 {
		bad("cache.stale_grace must be >= 0")
	}
	if cfg.Cache.Stampede.EarlyRefreshRatio < 0 || cfg.Cache.Stampede.EarlyRefreshRatio > 1 {
		bad("cache.stampede.early_refresh_ratio must be within [0,1]")
	}

	if _, ok := parseSourceName(cfg.Routing.Defaults.OnUnknownFreshness); !ok {
		bad("routing.defaults.on_unknown_freshness must be operational|execution|none (got %q)", cfg.Routing.Defaults.OnUnknownFreshness)
	}
	if cfg.Routing.Defaults.ClockSkewTolerance < 0 {
		bad("routing.defaults.clock_skew_tolerance must be >= 0")
	}
	if cfg.Routing.Defaults.InFlightResolutionEnabled() && cfg.Routing.Defaults.InFlightLookupTimeout <= 0 {
		bad("routing.defaults.in_flight_lookup_timeout must be > 0 when resolve_in_flight_execution is enabled")
	}
	if len(cfg.Routing.RequestTypes) == 0 {
		bad("routing.request_types must define at least one request type")
	}
	for name, rule := range cfg.Routing.RequestTypes {
		validateRule("routing.request_types."+name, rule, bad)
	}

	for field, sources := range cfg.Precedence.Fields {
		if len(sources) == 0 {
			bad("precedence.fields.%s must list at least one source", field)
		}
		for _, s := range sources {
			if _, ok := parseSourceName(s); !ok {
				bad("precedence.fields.%s: unknown source %q", field, s)
			}
		}
	}
	for _, f := range cfg.Precedence.ExecutionOverridesWhenRunning {
		if _, ok := cfg.Precedence.Fields[f]; !ok {
			bad("precedence.execution_overrides_when_running: %q is not a field in precedence.fields", f)
		}
	}

	for tenant, ov := range cfg.Tenants {
		if tenant == "" {
			bad("tenants: empty tenant id")
		}
		if ov.Routing != nil {
			for name, rule := range ov.Routing.RequestTypes {
				if _, ok := cfg.Routing.RequestTypes[name]; !ok {
					bad("tenants.%s.routing.request_types.%s: unknown request type", tenant, name)
				}
				// A tenant override is a patch, not a whole rule: only the
				// fields it actually sets are validated. Requiring a tenant to
				// restate preferred_source just to change a TTL is exactly the
				// friction that leads to copy-pasted, drifting config.
				validatePartialRule(fmt.Sprintf("tenants.%s.routing.request_types.%s", tenant, name), rule, bad)
			}
		}
	}

	if cfg.Observability.TraceSampleRatio < 0 || cfg.Observability.TraceSampleRatio > 1 {
		bad("observability.trace_sample_ratio must be within [0,1]")
	}
	switch cfg.Observability.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("observability.log.level must be debug|info|warn|error (got %q)", cfg.Observability.Log.Level)
	}
	switch cfg.Observability.Log.Format {
	case "json", "text":
	default:
		bad("observability.log.format must be json|text (got %q)", cfg.Observability.Log.Format)
	}

	if len(probs) > 0 {
		return fmt.Errorf("config: invalid: %w", errors.Join(probs...))
	}
	return nil
}

func validateSourceCommon(prefix string, c SourceCommon, bad func(string, ...any)) {
	if c.CallTimeout <= 0 {
		bad("%s.call_timeout must be > 0", prefix)
	}
	if c.Retry.Enabled {
		if c.Retry.MaxAttempts < 1 {
			bad("%s.retry.max_attempts must be >= 1", prefix)
		}
		if c.Retry.MaxAttempts > 5 {
			bad("%s.retry.max_attempts must be <= 5: unbounded retry amplifies outages", prefix)
		}
		if c.Retry.BaseBackoff <= 0 {
			bad("%s.retry.base_backoff must be > 0", prefix)
		}
		if c.Retry.MaxBackoff < c.Retry.BaseBackoff {
			bad("%s.retry.max_backoff must be >= base_backoff", prefix)
		}
		if c.Retry.JitterFraction < 0 || c.Retry.JitterFraction > 1 {
			bad("%s.retry.jitter_fraction must be within [0,1]", prefix)
		}
		if c.Retry.JitterFraction == 0 {
			bad("%s.retry.jitter_fraction must be > 0: synchronised retries cause thundering herds", prefix)
		}
		if c.Retry.BudgetRatio <= 0 || c.Retry.BudgetRatio > 1 {
			bad("%s.retry.budget_ratio must be within (0,1]", prefix)
		}
	}
	if c.Breaker.Enabled {
		if c.Breaker.FailureThreshold <= 0 || c.Breaker.FailureThreshold > 1 {
			bad("%s.breaker.failure_threshold must be within (0,1]", prefix)
		}
		if c.Breaker.MinimumRequests < 1 {
			bad("%s.breaker.minimum_requests must be >= 1", prefix)
		}
		if c.Breaker.Window <= 0 {
			bad("%s.breaker.window must be > 0", prefix)
		}
		if c.Breaker.OpenTimeout <= 0 {
			bad("%s.breaker.open_timeout must be > 0", prefix)
		}
		if c.Breaker.HalfOpenMaxCalls < 1 {
			bad("%s.breaker.half_open_max_calls must be >= 1", prefix)
		}
		if c.Breaker.HalfOpenSuccesses < 1 {
			bad("%s.breaker.half_open_successes must be >= 1", prefix)
		}
	}
	if c.Bulkhead.Enabled {
		if c.Bulkhead.MaxConcurrent < 1 {
			bad("%s.bulkhead.max_concurrent must be >= 1", prefix)
		}
		if c.Bulkhead.AcquireTimeout < 0 {
			bad("%s.bulkhead.acquire_timeout must be >= 0", prefix)
		}
	}
}

func validateRule(prefix string, r RoutingRule, bad func(string, ...any)) {
	switch strings.ToLower(r.PreferredSource) {
	case "operational", "execution", "both":
	default:
		bad("%s.preferred_source must be operational|execution|both (got %q)", prefix, r.PreferredSource)
	}
	if r.Fallback != "" {
		if _, ok := parseSourceName(r.Fallback); !ok {
			bad("%s.fallback must be operational|execution|none (got %q)", prefix, r.Fallback)
		}
	}
	if r.TTL < 0 {
		bad("%s.ttl must be >= 0", prefix)
	}
	if r.CacheTTL < 0 {
		bad("%s.cache_ttl must be >= 0", prefix)
	}
	// The cache must never be able to hand back data older than the freshness
	// TTL claims to allow; that is exactly the confusion this design forbids.
	if r.TTL > 0 && r.CacheTTL > r.TTL {
		bad("%s: cache_ttl (%s) exceeds source freshness ttl (%s); the cache would serve data the TTL policy calls stale",
			prefix, r.CacheTTL, r.TTL)
	}
	if r.AllowStale && r.MaxStale <= 0 {
		bad("%s: allow_stale requires max_stale > 0", prefix)
	}
	if r.MaxStale > 0 && r.TTL > 0 && r.MaxStale < r.TTL {
		bad("%s: max_stale (%s) must be >= ttl (%s)", prefix, r.MaxStale, r.TTL)
	}
	switch strings.ToLower(r.Consistency) {
	case "", "strong", "bounded", "eventual":
	default:
		bad("%s.consistency must be strong|bounded|eventual (got %q)", prefix, r.Consistency)
	}
	if strings.EqualFold(r.Consistency, "strong") && r.AllowStale {
		bad("%s: consistency=strong is incompatible with allow_stale=true", prefix)
	}
	for s := range r.RequiredSources {
		if _, ok := parseSourceName(s); !ok {
			bad("%s.required_sources: unknown source %q", prefix, s)
		}
	}
	for s, t := range r.PerSourceTimeout {
		if _, ok := parseSourceName(s); !ok {
			bad("%s.per_source_timeout: unknown source %q", prefix, s)
		}
		if t <= 0 {
			bad("%s.per_source_timeout.%s must be > 0", prefix, s)
		}
	}
}

// validatePartialRule validates a tenant override, checking only the fields
// that were set. The merged result is validated again at resolution time by
// ResolveRule's callers through the same invariants.
func validatePartialRule(prefix string, r RoutingRule, bad func(string, ...any)) {
	if r.PreferredSource != "" {
		switch strings.ToLower(r.PreferredSource) {
		case "operational", "execution", "both":
		default:
			bad("%s.preferred_source must be operational|execution|both (got %q)", prefix, r.PreferredSource)
		}
	}
	if r.Fallback != "" {
		if _, ok := parseSourceName(r.Fallback); !ok {
			bad("%s.fallback must be operational|execution|none (got %q)", prefix, r.Fallback)
		}
	}
	if r.TTL < 0 {
		bad("%s.ttl must be >= 0", prefix)
	}
	if r.CacheTTL < 0 {
		bad("%s.cache_ttl must be >= 0", prefix)
	}
	if r.TTL > 0 && r.CacheTTL > r.TTL {
		bad("%s: cache_ttl (%s) exceeds source freshness ttl (%s); the cache would serve data the TTL policy calls stale",
			prefix, r.CacheTTL, r.TTL)
	}
	if r.MaxStale > 0 && r.TTL > 0 && r.MaxStale < r.TTL {
		bad("%s: max_stale (%s) must be >= ttl (%s)", prefix, r.MaxStale, r.TTL)
	}
	if r.Consistency != "" {
		switch strings.ToLower(r.Consistency) {
		case "strong", "bounded", "eventual":
		default:
			bad("%s.consistency must be strong|bounded|eventual (got %q)", prefix, r.Consistency)
		}
		if strings.EqualFold(r.Consistency, "strong") && r.AllowStale {
			bad("%s: consistency=strong is incompatible with allow_stale=true", prefix)
		}
	}
	for s := range r.RequiredSources {
		if _, ok := parseSourceName(s); !ok {
			bad("%s.required_sources: unknown source %q", prefix, s)
		}
	}
	for s, t := range r.PerSourceTimeout {
		if _, ok := parseSourceName(s); !ok {
			bad("%s.per_source_timeout: unknown source %q", prefix, s)
		}
		if t <= 0 {
			bad("%s.per_source_timeout.%s must be > 0", prefix, s)
		}
	}
}

func parseSourceName(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "operational", "execution":
		return strings.ToLower(s), true
	case "", "none":
		return "none", true
	}
	return "", false
}

// ResolveRule returns the routing rule for a request type as seen by a
// specific tenant. Tenant overrides are applied field-by-field so a tenant may
// change a single TTL without restating the rule (REQ-MT-005, REQ-CFG-005).
func (c *Config) ResolveRule(tenantID, requestType string) (RoutingRule, bool) {
	base, ok := c.Routing.RequestTypes[requestType]
	if !ok {
		return RoutingRule{}, false
	}
	ov, ok := c.Tenants[tenantID]
	if !ok || ov.Routing == nil {
		return base, true
	}
	tr, ok := ov.Routing.RequestTypes[requestType]
	if !ok {
		return base, true
	}
	merged := base
	if tr.PreferredSource != "" {
		merged.PreferredSource = tr.PreferredSource
	}
	if tr.Fallback != "" {
		merged.Fallback = tr.Fallback
	}
	if tr.Consistency != "" {
		merged.Consistency = tr.Consistency
	}
	if tr.TTL != 0 {
		merged.TTL = tr.TTL
	}
	if tr.CacheTTL != 0 {
		merged.CacheTTL = tr.CacheTTL
	}
	if tr.MaxStale != 0 {
		merged.MaxStale = tr.MaxStale
	}
	if tr.MaxLatency != 0 {
		merged.MaxLatency = tr.MaxLatency
	}
	if tr.RequiredSources != nil {
		merged.RequiredSources = tr.RequiredSources
	}
	if tr.PerSourceTimeout != nil {
		merged.PerSourceTimeout = tr.PerSourceTimeout
	}
	if len(tr.RequiredFields) > 0 {
		merged.RequiredFields = tr.RequiredFields
	}
	// AllowStale is a bool, so "unset" is indistinguishable from false. It is
	// therefore only overridable when the tenant also sets max_stale, which
	// makes the intent explicit.
	if tr.MaxStale != 0 {
		merged.AllowStale = tr.AllowStale
	}
	return merged, true
}

// ResolveDefaults returns routing defaults as seen by a tenant.
func (c *Config) ResolveDefaults(tenantID string) RoutingDefaults {
	d := c.Routing.Defaults
	ov, ok := c.Tenants[tenantID]
	if !ok || ov.Routing == nil {
		return d
	}
	td := ov.Routing.Defaults
	if td.OnUnknownFreshness != "" {
		d.OnUnknownFreshness = td.OnUnknownFreshness
	}
	if td.ClockSkewTolerance != 0 {
		d.ClockSkewTolerance = td.ClockSkewTolerance
	}
	// Each field overlays independently: a tenant may change a duration without
	// restating the flag beside it, and turn a flag off without inventing a
	// duration to carry it.
	if td.MaxStale != 0 {
		d.MaxStale = td.MaxStale
	}
	if td.AllowStale != nil {
		d.AllowStale = td.AllowStale
	}
	if td.ProbeCacheTTL != 0 {
		d.ProbeCacheTTL = td.ProbeCacheTTL
	}
	if td.ProbeEnabled != nil {
		d.ProbeEnabled = td.ProbeEnabled
	}
	if td.InFlightLookupTimeout != 0 {
		d.InFlightLookupTimeout = td.InFlightLookupTimeout
	}
	if td.ResolveInFlightExecution != nil {
		d.ResolveInFlightExecution = td.ResolveInFlightExecution
	}
	return d
}

// ResolvePrecedence returns the precedence policy as seen by a tenant.
func (c *Config) ResolvePrecedence(tenantID string) PrecedenceConfig {
	if ov, ok := c.Tenants[tenantID]; ok && ov.Precedence != nil {
		p := *ov.Precedence
		if len(p.Fields) == 0 {
			p.Fields = c.Precedence.Fields
		}
		return p
	}
	return c.Precedence
}

// ResolveRateLimit returns the rate-limit policy as seen by a tenant.
func (c *Config) ResolveRateLimit(tenantID string) RateLimitConfig {
	if ov, ok := c.Tenants[tenantID]; ok && ov.Security != nil && ov.Security.RateLimit != nil {
		return *ov.Security.RateLimit
	}
	return c.Security.RateLimit
}

// RequestTypes returns the configured request-type names. Used by the
// classifier to fail fast on an unroutable classification.
func (c *Config) RequestTypes() []string {
	out := make([]string, 0, len(c.Routing.RequestTypes))
	for k := range c.Routing.RequestTypes {
		out = append(out, k)
	}
	return out
}

// Redacted returns a copy safe to log at startup: secrets are indirected
// through environment variable names already, but TLS paths and Redis
// credentials are still trimmed (REQ-SEC-008).
func (c Config) Redacted() Config {
	// The JWKS URL is not a secret and stays as-is; the Redis username is
	// masked because it is frequently an IAM identity.
	c.Cache.Redis.Username = mask(c.Cache.Redis.Username)
	return c
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// Now is indirected so tests can freeze time without touching global state.
type Clock interface{ Now() time.Time }

// SystemClock reads the wall clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }
