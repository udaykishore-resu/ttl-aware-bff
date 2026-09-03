// Package config is the only place in the service where a TTL, a timeout or a
// threshold is allowed to have a value. Nothing under internal/ may compile a
// duration into a literal; every number reaches the code through this package.
//
// Resolution order, lowest precedence first:
//  1. built-in defaults (Default())
//  2. YAML file
//  3. environment overrides (BFF_ prefix, "__" nesting separator)
//  4. per-tenant overrides (applied at request time, not at load time)
//
// Traceability: REQ-CFG-001..REQ-CFG-009, REQ-MT-005.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Root
// ---------------------------------------------------------------------------

// Config is the fully resolved service configuration.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Security      SecurityConfig      `yaml:"security"`
	Sources       SourcesConfig       `yaml:"sources"`
	Cache         CacheConfig         `yaml:"cache"`
	Routing       RoutingConfig       `yaml:"routing"`
	Precedence    PrecedenceConfig    `yaml:"precedence"`
	Tenants       map[string]Override `yaml:"tenants"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// Override is the subset of configuration a tenant may specialise. Anything
// absent falls through to the global value (REQ-MT-005).
type Override struct {
	Routing    *RoutingConfig    `yaml:"routing,omitempty"`
	Cache      *CacheOverride    `yaml:"cache,omitempty"`
	Security   *SecurityOverride `yaml:"security,omitempty"`
	Precedence *PrecedenceConfig `yaml:"precedence,omitempty"`
}

// CacheOverride is the tenant-tunable subset of the cache configuration.
type CacheOverride struct {
	Enabled     *bool     `yaml:"enabled,omitempty"`
	NegativeTTL *Duration `yaml:"negative_ttl,omitempty"`
}

// SecurityOverride is the tenant-tunable subset of the security configuration.
type SecurityOverride struct {
	RateLimit *RateLimitConfig `yaml:"rate_limit,omitempty"`
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// ServerConfig governs the inbound HTTP surface.
type ServerConfig struct {
	HTTPAddr      string   `yaml:"http_addr"`
	AdminAddr     string   `yaml:"admin_addr"`
	ReadTimeout   Duration `yaml:"read_timeout"`
	WriteTimeout  Duration `yaml:"write_timeout"`
	IdleTimeout   Duration `yaml:"idle_timeout"`
	HeaderTimeout Duration `yaml:"header_timeout"`
	ShutdownGrace Duration `yaml:"shutdown_grace"`
	MaxBodyBytes  int64    `yaml:"max_body_bytes"`
	// RequestTimeout is the total budget for one API request, enforced by
	// middleware. Per-source timeouts are carved out of what remains.
	RequestTimeout Duration `yaml:"request_timeout"`
}

// ---------------------------------------------------------------------------
// Security
// ---------------------------------------------------------------------------

// SecurityConfig covers authentication, authorisation and admission control.
type SecurityConfig struct {
	JWT       JWTConfig       `yaml:"jwt"`
	RBAC      RBACConfig      `yaml:"rbac"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	// AllowInsecureNoAuth disables authentication. It exists so the local
	// compose stack is usable, and is refused when environment != "local".
	AllowInsecureNoAuth bool `yaml:"allow_insecure_no_auth"`
}

// JWTConfig configures bearer-token verification.
type JWTConfig struct {
	Issuer   string   `yaml:"issuer"`
	Audience string   `yaml:"audience"`
	JWKSURL  string   `yaml:"jwks_url"`
	JWKSTTL  Duration `yaml:"jwks_ttl"`
	// HS256SecretEnv names the environment variable holding a shared secret.
	// The secret itself is never written to the config file (REQ-SEC-008).
	HS256SecretEnv string   `yaml:"hs256_secret_env"`
	Leeway         Duration `yaml:"leeway"`
	TenantClaim    string   `yaml:"tenant_claim"`
	RolesClaim     string   `yaml:"roles_claim"`
	RequiredClaims []string `yaml:"required_claims"`
}

// RBACConfig maps roles to the permissions they grant.
type RBACConfig struct {
	// Roles maps a role name to the permissions it grants, e.g.
	// "resource.read", "execution.read", "execution.audit.read".
	Roles map[string][]string `yaml:"roles"`
	// DefaultDeny, when true, rejects any request whose required permission is
	// not explicitly granted. Always true in production.
	DefaultDeny bool `yaml:"default_deny"`
}

// RateLimitConfig is a token-bucket configuration.
type RateLimitConfig struct {
	Enabled   bool `yaml:"enabled"`
	RPS       int  `yaml:"rps"`
	Burst     int  `yaml:"burst"`
	PerTenant bool `yaml:"per_tenant"`
	// MaxTenants bounds the limiter map so a hostile tenant id cannot grow it
	// without limit.
	MaxTenants int `yaml:"max_tenants"`
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// SourcesConfig holds one block per data source.
type SourcesConfig struct {
	Operational OperationalSourceConfig `yaml:"operational"`
	Execution   ExecutionSourceConfig   `yaml:"execution"`
}

// SourceCommon is the resilience configuration every source shares.
type SourceCommon struct {
	CallTimeout Duration       `yaml:"call_timeout"`
	Retry       RetryConfig    `yaml:"retry"`
	Breaker     BreakerConfig  `yaml:"breaker"`
	Bulkhead    BulkheadConfig `yaml:"bulkhead"`
	TLS         TLSConfig      `yaml:"tls"`
	// AcceptedSchemaVersions gates responses whose declared schema version the
	// BFF has not been built against (REQ-EDGE-017). Empty means accept all.
	AcceptedSchemaVersions []string `yaml:"accepted_schema_versions"`
}

// OperationalSourceConfig configures the gRPC ODS client.
type OperationalSourceConfig struct {
	SourceCommon      `yaml:",inline"`
	Addr              string   `yaml:"addr"`
	DialTimeout       Duration `yaml:"dial_timeout"`
	ProbeTimeout      Duration `yaml:"freshness_probe_timeout"`
	KeepaliveTime     Duration `yaml:"keepalive_time"`
	KeepaliveTimeout  Duration `yaml:"keepalive_timeout"`
	MaxRecvMsgBytes   int      `yaml:"max_recv_msg_bytes"`
	UseRoundRobin     bool     `yaml:"use_round_robin"`
	WaitForReadyCalls bool     `yaml:"wait_for_ready"`
}

// ExecutionSourceConfig configures the REST EDS client.
type ExecutionSourceConfig struct {
	SourceCommon        `yaml:",inline"`
	BaseURL             string   `yaml:"base_url"`
	MaxIdleConns        int      `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int      `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout     Duration `yaml:"idle_conn_timeout"`
	HistoryPageSize     int      `yaml:"history_page_size"`
	MaxHistoryItems     int      `yaml:"max_history_items"`
}

// RetryConfig bounds retry behaviour. Blind retries are impossible by
// construction: MaxAttempts is bounded and jitter is mandatory (REQ-RES-004).
type RetryConfig struct {
	Enabled     bool     `yaml:"enabled"`
	MaxAttempts int      `yaml:"max_attempts"`
	BaseBackoff Duration `yaml:"base_backoff"`
	MaxBackoff  Duration `yaml:"max_backoff"`
	// JitterFraction in [0,1]; 1.0 means full jitter.
	JitterFraction float64 `yaml:"jitter_fraction"`
	// PerAttemptTimeout caps each individual attempt so that a retry budget
	// cannot consume the whole request deadline in one attempt.
	PerAttemptTimeout Duration `yaml:"per_attempt_timeout"`
	// BudgetRatio caps total retry time as a fraction of the call timeout.
	BudgetRatio float64 `yaml:"budget_ratio"`
}

// BreakerConfig configures the three-state circuit breaker.
type BreakerConfig struct {
	Enabled bool `yaml:"enabled"`
	// FailureThreshold is the failure ratio in [0,1] that trips the breaker.
	FailureThreshold float64 `yaml:"failure_threshold"`
	// MinimumRequests is the sample floor before the ratio is meaningful.
	MinimumRequests int `yaml:"minimum_requests"`
	// Window is the rolling evaluation window.
	Window Duration `yaml:"window"`
	// OpenTimeout is how long the breaker stays open before probing.
	OpenTimeout Duration `yaml:"open_timeout"`
	// HalfOpenMaxCalls bounds concurrent probes in half-open state.
	HalfOpenMaxCalls int `yaml:"half_open_max_calls"`
	// HalfOpenSuccesses required to close the breaker again.
	HalfOpenSuccesses int `yaml:"half_open_successes"`
}

// BulkheadConfig bounds concurrency per source so a slow source cannot exhaust
// the BFF's goroutines or connections (REQ-RES-005).
type BulkheadConfig struct {
	Enabled        bool     `yaml:"enabled"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	AcquireTimeout Duration `yaml:"acquire_timeout"`
	MaxQueue       int      `yaml:"max_queue"`
}

// TLSConfig configures transport security to a source.
type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	ServerName         string `yaml:"server_name"`
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// CacheConfig configures the BFF's own cache. Note that no field here is a
// source freshness TTL: cache lifetime and data freshness are independent
// dimensions and are configured in different blocks on purpose.
type CacheConfig struct {
	Enabled   bool        `yaml:"enabled"`
	Backend   string      `yaml:"backend"` // memory | redis | layered
	Redis     RedisConfig `yaml:"redis"`
	L1        L1Config    `yaml:"l1"`
	KeyPrefix string      `yaml:"key_prefix"`
	// NegativeTTL is how long a "not found" is remembered (REQ-CACHE-007).
	NegativeTTL Duration `yaml:"negative_ttl"`
	// StaleGrace is how long an entry is physically retained *past* its cache
	// TTL so that it remains available as a last-resort degraded answer when
	// every source is down. A logically expired entry is never served on the
	// normal path; only the explicit stale-serve path can reach it
	// (REQ-RES-007, REQ-EDGE-005).
	StaleGrace Duration       `yaml:"stale_grace"`
	Stampede   StampedeConfig `yaml:"stampede"`
	// FailOpen: when the cache backend errors, serve the request from source
	// rather than failing it. Always true in production.
	FailOpen bool `yaml:"fail_open"`
}

// RedisConfig configures the L2 distributed cache.
type RedisConfig struct {
	Addr         string    `yaml:"addr"`
	DB           int       `yaml:"db"`
	Username     string    `yaml:"username"`
	PasswordEnv  string    `yaml:"password_env"`
	PoolSize     int       `yaml:"pool_size"`
	MinIdle      int       `yaml:"min_idle_conns"`
	DialTimeout  Duration  `yaml:"dial_timeout"`
	ReadTimeout  Duration  `yaml:"read_timeout"`
	WriteTimeout Duration  `yaml:"write_timeout"`
	TLS          TLSConfig `yaml:"tls"`
}

// L1Config configures the in-process cache tier.
type L1Config struct {
	Enabled    bool     `yaml:"enabled"`
	MaxEntries int      `yaml:"max_entries"`
	TTL        Duration `yaml:"ttl"`
}

// StampedeConfig configures protection against concurrent identical misses
// (REQ-CACHE-008, REQ-EDGE-012).
type StampedeConfig struct {
	Enabled bool     `yaml:"enabled"`
	LockTTL Duration `yaml:"lock_ttl"`
	// EarlyRefreshRatio triggers a background refresh once an entry has spent
	// this fraction of its life, smoothing the expiry cliff.
	EarlyRefreshRatio float64 `yaml:"early_refresh_ratio"`
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// RoutingConfig is the policy input for the DataSourceRouter.
type RoutingConfig struct {
	Defaults     RoutingDefaults        `yaml:"defaults"`
	RequestTypes map[string]RoutingRule `yaml:"request_types"`
}

// RoutingDefaults apply wherever a rule does not specify.
//
// The booleans here are POINTERS, which is not decoration. A tenant override is
// a patch, and a plain bool has no "absent" state: `allow_stale: false` in a
// tenant block would be indistinguishable from not mentioning it, so the
// overlay could only apply it by also requiring some companion duration to
// change. Pointers make "the tenant said false" and "the tenant said nothing"
// different things, which is the whole point of an override (REQ-MT-005).
type RoutingDefaults struct {
	// OnUnknownFreshness is the source chosen when freshness cannot be
	// established at all (REQ-TTL-006).
	OnUnknownFreshness string   `yaml:"on_unknown_freshness"`
	ClockSkewTolerance Duration `yaml:"clock_skew_tolerance"`
	AllowStale         *bool    `yaml:"allow_stale,omitempty"`
	MaxStale           Duration `yaml:"max_stale"`
	// ProbeEnabled turns the cheap pre-fetch freshness probe on or off. With it
	// off, freshness is evaluated post-fetch only.
	ProbeEnabled *bool `yaml:"probe_enabled,omitempty"`
	// ProbeCacheTTL is how long a probe result is reused. It is a cache TTL,
	// not a freshness TTL: it bounds how often we ask, not how old data may be.
	ProbeCacheTTL Duration `yaml:"probe_cache_ttl"`
	// ResolveInFlightExecution makes an operational-only read consult the
	// execution source when, and only when, the operational record says a
	// workflow is currently mutating the resource.
	//
	// Without it the precedence rule "a running execution may override
	// operational state" can only fire on endpoints that already read both
	// sources, so /status and /details would report different statuses for the
	// same resource during a workflow. With it, the extra call happens only in
	// the rare in-flight case and is optional: if it fails, the operational
	// answer stands (REQ-PREC-003, REQ-EDGE-015).
	ResolveInFlightExecution *bool `yaml:"resolve_in_flight_execution,omitempty"`
	// InFlightLookupTimeout bounds that extra call. It is deliberately much
	// tighter than the execution source's normal budget: this is a hot-path
	// enrichment, not a read the response depends on.
	InFlightLookupTimeout Duration `yaml:"in_flight_lookup_timeout"`
}

// RoutingRule is the per-request-type policy. Every duration here is a
// configuration value; none of them appears as a literal anywhere else.
type RoutingRule struct {
	// PreferredSource: operational | execution | both.
	PreferredSource string `yaml:"preferred_source"`
	// TTL is the SOURCE FRESHNESS TTL: how old operational data may be and
	// still satisfy this request type. Zero means "never trust cached-age
	// state, always read the preferred source live".
	TTL Duration `yaml:"ttl"`
	// CacheTTL is the BFF RESPONSE CACHE TTL. Unrelated to TTL above.
	CacheTTL Duration `yaml:"cache_ttl"`
	// Fallback is the source used when the preferred one cannot serve.
	Fallback string `yaml:"fallback"`
	// AllowStale permits serving operational data past its TTL when no
	// alternative exists, marking the response degraded.
	AllowStale bool `yaml:"allow_stale"`
	// MaxStale is the hard ceiling past which stale data is refused outright.
	MaxStale Duration `yaml:"max_stale"`
	// Consistency: strong | bounded | eventual.
	Consistency string `yaml:"consistency"`
	// RequiredSources marks which sources must succeed for a BOTH request.
	// A source mapped to false may fail and yield a partial response.
	RequiredSources map[string]bool `yaml:"required_sources"`
	// PerSourceTimeout overrides the source's default call timeout for this
	// request type, letting a cheap endpoint hold a tighter budget.
	PerSourceTimeout map[string]Duration `yaml:"per_source_timeout"`
	// RequiredFields, when set, tells the router which canonical fields the
	// response must carry; the field catalogue turns that into a source set.
	RequiredFields []string `yaml:"required_fields"`
	// MaxLatency expresses the UI's latency requirement for this request type.
	MaxLatency Duration `yaml:"max_latency"`
}

// StaleAllowed reports whether stale data may be served, defaulting to true.
func (d RoutingDefaults) StaleAllowed() bool { return boolOr(d.AllowStale, true) }

// ProbesEnabled reports whether the pre-fetch freshness probe runs, defaulting
// to true.
func (d RoutingDefaults) ProbesEnabled() bool { return boolOr(d.ProbeEnabled, true) }

// InFlightResolutionEnabled reports whether an operational-only read consults
// the execution source for a resource with a workflow in flight, defaulting to
// true.
func (d RoutingDefaults) InFlightResolutionEnabled() bool {
	return boolOr(d.ResolveInFlightExecution, true)
}

// Bool returns a pointer to v, for building configuration in Default() and in
// tests.
func Bool(v bool) *bool { return &v }

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// ---------------------------------------------------------------------------
// Precedence
// ---------------------------------------------------------------------------

// PrecedenceConfig makes source precedence explicit and inspectable. Nothing
// in the code may prefer one source over another except through this
// (REQ-PREC-001).
type PrecedenceConfig struct {
	// Fields maps a canonical field name to an ordered list of sources,
	// most-authoritative first.
	Fields map[string][]string `yaml:"fields"`
	// ExecutionOverridesWhenRunning lists the fields for which a running
	// execution outranks the operational source (REQ-PREC-003).
	ExecutionOverridesWhenRunning []string `yaml:"execution_overrides_when_running"`
	// ConflictWarning emits a response warning whenever two sources disagreed
	// on a field and precedence had to choose.
	ConflictWarning bool `yaml:"conflict_warning"`
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

// ObservabilityConfig configures traces, metrics and logs.
type ObservabilityConfig struct {
	ServiceName      string     `yaml:"service_name"`
	ServiceVersion   string     `yaml:"service_version"`
	Environment      string     `yaml:"environment"`
	OTLP             OTLPConfig `yaml:"otlp"`
	TraceSampleRatio float64    `yaml:"trace_sample_ratio"`
	MetricsInterval  Duration   `yaml:"metrics_interval"`
	Log              LogConfig  `yaml:"log"`
	// PrometheusEndpoint enables the in-process /metrics exposition.
	PrometheusEndpoint bool `yaml:"prometheus_endpoint"`
}

// OTLPConfig configures the OTLP/gRPC exporter.
type OTLPConfig struct {
	Enabled  bool              `yaml:"enabled"`
	Endpoint string            `yaml:"endpoint"`
	Insecure bool              `yaml:"insecure"`
	Timeout  Duration          `yaml:"timeout"`
	Headers  map[string]string `yaml:"headers"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
	// AddSource includes file:line. Off in production: it is expensive.
	AddSource bool `yaml:"add_source"`
	// RedactKeys are log attribute keys whose values are replaced (REQ-SEC-009).
	RedactKeys []string `yaml:"redact_keys"`
}

// ---------------------------------------------------------------------------
// Duration: a YAML/env friendly time.Duration
// ---------------------------------------------------------------------------

// Duration wraps time.Duration so that "30s" in YAML and "30s" in an
// environment variable parse identically, and so that a bare number is an
// error rather than silently meaning nanoseconds.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the Go duration form.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string such as "30s" or "1m30s".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	return d.Parse(s)
}

// MarshalYAML renders the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Parse sets the duration from a string.
func (d *Duration) Parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	// Accept a bare "0" for readability of "always live" TTLs.
	if s == "0" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// ---------------------------------------------------------------------------
// Environment overrides
// ---------------------------------------------------------------------------

const envPrefix = "BFF_"
const envSeparator = "__"

// applyEnv walks the environment for BFF_-prefixed variables and applies them
// to the parsed YAML tree before decoding. Working on the YAML node tree keeps
// one code path for both sources of truth and means a typo in an override is
// reported as a config error instead of being silently ignored.
func applyEnv(root *yaml.Node, environ []string) error {
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, envPrefix) {
			continue
		}
		path := strings.Split(strings.ToLower(strings.TrimPrefix(key, envPrefix)), strings.ToLower(envSeparator))
		if len(path) == 0 || path[0] == "" {
			continue
		}
		if err := setYAMLPath(root, path, val); err != nil {
			return fmt.Errorf("env override %s: %w", key, err)
		}
	}
	return nil
}

// setYAMLPath creates or replaces a scalar at the given mapping path.
func setYAMLPath(node *yaml.Node, path []string, value string) error {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		}
		return setYAMLPath(node.Content[0], path, value)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("cannot set %q: parent is not a mapping", strings.Join(path, "."))
	}
	head := path[0]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != head {
			continue
		}
		if len(path) == 1 {
			node.Content[i+1] = scalarNode(value)
			return nil
		}
		return setYAMLPath(node.Content[i+1], path[1:], value)
	}
	// Key absent: create it.
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: head})
	if len(path) == 1 {
		node.Content = append(node.Content, scalarNode(value))
		return nil
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content, child)
	return setYAMLPath(child, path[1:], value)
}

// scalarNode types the value so YAML decoding produces the right Go kind.
// Strings that look like durations stay strings, which is what Duration wants.
func scalarNode(v string) *yaml.Node {
	tag := "!!str"
	switch {
	case v == "true" || v == "false":
		tag = "!!bool"
	case isInt(v):
		tag = "!!int"
	case isFloat(v):
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
}

func isInt(v string) bool {
	_, err := strconv.ParseInt(v, 10, 64)
	return err == nil
}

func isFloat(v string) bool {
	if isInt(v) {
		return false
	}
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// EnvKeyFor renders the environment-variable name for a dotted config path.
// Exported so documentation and tests cannot disagree with the loader.
func EnvKeyFor(dotted string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(dotted, ".", envSeparator))
}

// SecretFromEnv reads a secret indirection. An empty env name yields "".
func SecretFromEnv(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}
