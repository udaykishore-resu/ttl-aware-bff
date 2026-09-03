package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// ---------------------------------------------------------------------------
// Duration
// ---------------------------------------------------------------------------

// TestDuration_UnmarshalYAML verifies REQ-CFG-003: durations are parsed from
// duration strings, a bare "0" is accepted for readability, and a bare number
// is a configuration error rather than a silent nanosecond count. A non-scalar
// node is rejected with the "must be a string" message.
func TestDuration_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	type holder struct {
		TTL Duration `yaml:"ttl"`
	}

	cases := []struct {
		name    string
		doc     string
		want    time.Duration
		wantErr string
	}{
		{name: "seconds", doc: `ttl: "30s"`, want: 30 * time.Second},
		{name: "compound", doc: `ttl: "1m30s"`, want: 90 * time.Second},
		{name: "milliseconds", doc: `ttl: "250ms"`, want: 250 * time.Millisecond},
		{name: "bare zero string", doc: `ttl: "0"`, want: 0},
		{name: "unquoted zero", doc: `ttl: 0`, want: 0},
		{name: "empty string", doc: `ttl: ""`, want: 0},
		{name: "whitespace only", doc: `ttl: "   "`, want: 0},
		{name: "invalid text", doc: `ttl: "soon"`, wantErr: `invalid duration "soon"`},
		// A bare number is refused: "30" is not 30 nanoseconds, it is a mistake.
		{name: "bare number", doc: `ttl: 30`, wantErr: `invalid duration "30"`},
		{name: "quoted bare number", doc: `ttl: "30"`, wantErr: `invalid duration "30"`},
		{name: "float", doc: `ttl: 1.5`, wantErr: `invalid duration "1.5"`},
		{name: "bool", doc: `ttl: true`, wantErr: `invalid duration "true"`},
		{name: "mapping", doc: "ttl:\n  value: 30s", wantErr: "duration must be a string"},
		{name: "sequence", doc: "ttl:\n  - 30s", wantErr: "duration must be a string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var h holder
			err := yaml.Unmarshal([]byte(tc.doc), &h)
			if tc.wantErr != "" {
				testutil.Error(t, err, "expected %q to be rejected", tc.doc)
				testutil.True(t, strings.Contains(err.Error(), tc.wantErr),
					"error %q should mention %q", err.Error(), tc.wantErr)
				return
			}
			testutil.NoError(t, err, "parsing %q", tc.doc)
			testutil.Equal(t, h.TTL.D(), tc.want, "parsed duration for %q", tc.doc)
		})
	}
}

// TestDuration_Accessors verifies the small surface Duration exposes to the
// rest of the service (REQ-CFG-001).
func TestDuration_Accessors(t *testing.T) {
	t.Parallel()

	var d Duration
	testutil.NoError(t, d.Parse("1500ms"), "parse")
	testutil.Equal(t, d.D(), 1500*time.Millisecond, "D()")
	testutil.Equal(t, d.String(), "1.5s", "String()")

	out, err := d.MarshalYAML()
	testutil.NoError(t, err, "MarshalYAML")
	testutil.Equal(t, out.(string), "1.5s", "marshalled form")

	testutil.Error(t, d.Parse("nope"), "an unparseable duration must be rejected")
}

// ---------------------------------------------------------------------------
// Environment overrides
// ---------------------------------------------------------------------------

// TestLoadWithEnv_Overrides verifies REQ-CFG-002: BFF_-prefixed variables
// override the file layer, nest through "__", create keys the document does not
// contain, and are typed so that bools, ints, floats, strings and durations all
// decode into the right Go kind.
func TestLoadWithEnv_Overrides(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		environ []string
		check   func(t *testing.T, cfg Config)
	}{
		{
			name:    "string",
			environ: []string{"BFF_SERVER__HTTP_ADDR=:19999"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Server.HTTPAddr, ":19999", "server.http_addr")
			},
		},
		{
			name:    "bool false",
			environ: []string{"BFF_CACHE__ENABLED=false"},
			check: func(t *testing.T, cfg Config) {
				testutil.False(t, cfg.Cache.Enabled, "cache.enabled must be coerced to a bool")
			},
		},
		{
			name:    "bool true",
			environ: []string{"BFF_OBSERVABILITY__LOG__ADD_SOURCE=true"},
			check: func(t *testing.T, cfg Config) {
				testutil.True(t, cfg.Observability.Log.AddSource, "log.add_source must be coerced to a bool")
			},
		},
		{
			name:    "int",
			environ: []string{"BFF_SOURCES__EXECUTION__HISTORY_PAGE_SIZE=42"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Sources.Execution.HistoryPageSize, 42, "history_page_size")
			},
		},
		{
			name:    "int64",
			environ: []string{"BFF_SERVER__MAX_BODY_BYTES=4096"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Server.MaxBodyBytes, int64(4096), "max_body_bytes")
			},
		},
		{
			name:    "float",
			environ: []string{"BFF_OBSERVABILITY__TRACE_SAMPLE_RATIO=0.25"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Observability.TraceSampleRatio, 0.25, "trace_sample_ratio")
			},
		},
		{
			name:    "duration stays a string",
			environ: []string{"BFF_SERVER__REQUEST_TIMEOUT=12s"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Server.RequestTimeout.D(), 12*time.Second, "request_timeout")
			},
		},
		{
			name: "deep nesting creates intermediate mappings",
			environ: []string{
				"BFF_SOURCES__OPERATIONAL__BREAKER__MINIMUM_REQUESTS=7",
				"BFF_SOURCES__OPERATIONAL__BREAKER__OPEN_TIMEOUT=45s",
			},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Sources.Operational.Breaker.MinimumRequests, 7, "breaker.minimum_requests")
				testutil.Equal(t, cfg.Sources.Operational.Breaker.OpenTimeout.D(), 45*time.Second, "breaker.open_timeout")
				// A sibling the override did not name keeps its default.
				testutil.Equal(t, cfg.Sources.Operational.Breaker.HalfOpenSuccesses, 2, "untouched sibling")
			},
		},
		{
			name:    "non-BFF variables are ignored",
			environ: []string{"PATH=/usr/bin", "HOME=/root", "BFFISH_SERVER__HTTP_ADDR=:1"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Server.HTTPAddr, Default().Server.HTTPAddr, "http_addr must be untouched")
			},
		},
		{
			name:    "malformed environ entries are skipped",
			environ: []string{"NOEQUALSSIGN", "BFF_=x", "BFF_SERVER__HTTP_ADDR=:18080"},
			check: func(t *testing.T, cfg Config) {
				testutil.Equal(t, cfg.Server.HTTPAddr, ":18080", "well-formed override still applies")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadWithEnv("", tc.environ)
			testutil.NoError(t, err, "LoadWithEnv")
			tc.check(t, cfg)
		})
	}
}

// TestLoadWithEnv_OverridesFile verifies REQ-CFG-002's precedence order: the
// environment beats the file, and the file beats the built-in defaults.
func TestLoadWithEnv_OverridesFile(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
server:
  http_addr: ":7001"
  read_timeout: "9s"
`)

	fileOnly, err := LoadWithEnv(path, nil)
	testutil.NoError(t, err, "load file only")
	testutil.Equal(t, fileOnly.Server.HTTPAddr, ":7001", "file beats defaults")
	testutil.Equal(t, fileOnly.Server.ReadTimeout.D(), 9*time.Second, "file duration")
	testutil.Equal(t, fileOnly.Server.AdminAddr, Default().Server.AdminAddr, "unspecified key keeps its default")

	both, err := LoadWithEnv(path, []string{"BFF_SERVER__HTTP_ADDR=:7002"})
	testutil.NoError(t, err, "load file plus env")
	testutil.Equal(t, both.Server.HTTPAddr, ":7002", "env beats file")
	testutil.Equal(t, both.Server.ReadTimeout.D(), 9*time.Second, "file value the env did not touch survives")
}

// TestLoadWithEnv_RejectsBadOverride verifies REQ-CFG-002/REQ-CFG-003: an
// override that cannot be applied to the document, or whose value does not
// decode, is a load error rather than a silently ignored variable.
func TestLoadWithEnv_RejectsBadOverride(t *testing.T) {
	t.Parallel()

	// server.http_addr is already a scalar in the document; descending into it
	// is impossible and must be reported rather than silently ignored.
	path := writeConfig(t, "server:\n  http_addr: \":7001\"\n")
	_, err := LoadWithEnv(path, []string{"BFF_SERVER__HTTP_ADDR__NESTED=x"})
	testutil.Error(t, err, "descending into a scalar must fail")
	testutil.True(t, strings.Contains(err.Error(), "parent is not a mapping"),
		"error should explain the shape problem, got %q", err.Error())

	// A key the document does not contain is created, so the same override
	// against an empty document fails later, at decode time.
	_, err = LoadWithEnv("", []string{"BFF_SERVER__HTTP_ADDR__NESTED=x"})
	testutil.Error(t, err, "an override of the wrong shape must fail the load")

	_, err = LoadWithEnv("", []string{"BFF_SERVER__REQUEST_TIMEOUT=soon"})
	testutil.Error(t, err, "an unparseable duration override must fail the load")
}

// TestEnvKeyFor verifies REQ-CFG-002: the documented environment key for a
// dotted path is produced by the loader's own helper, so documentation and
// implementation cannot drift.
func TestEnvKeyFor(t *testing.T) {
	t.Parallel()

	cases := []struct{ dotted, want string }{
		{"server.http_addr", "BFF_SERVER__HTTP_ADDR"},
		{"sources.operational.addr", "BFF_SOURCES__OPERATIONAL__ADDR"},
		{"routing.request_types.resource_status.ttl", "BFF_ROUTING__REQUEST_TYPES__RESOURCE_STATUS__TTL"},
		{"cache", "BFF_CACHE"},
		{"", "BFF_"},
	}
	for _, tc := range cases {
		t.Run(tc.dotted, func(t *testing.T) {
			t.Parallel()
			testutil.Equal(t, EnvKeyFor(tc.dotted), tc.want, "EnvKeyFor(%q)", tc.dotted)
		})
	}
}

// TestEnvKeyFor_RoundTrips verifies that a key produced by EnvKeyFor is one the
// loader actually honours (REQ-CFG-002).
func TestEnvKeyFor_RoundTrips(t *testing.T) {
	t.Parallel()

	cfg, err := LoadWithEnv("", []string{EnvKeyFor("sources.operational.addr") + "=ods.example:9101"})
	testutil.NoError(t, err, "LoadWithEnv")
	testutil.Equal(t, cfg.Sources.Operational.Addr, "ods.example:9101", "addr override")
}

// TestSecretFromEnv verifies REQ-SEC-008: secrets are indirected through an
// environment variable name, and an unset indirection yields "".
func TestSecretFromEnv(t *testing.T) {
	testutil.Equal(t, SecretFromEnv(""), "", "an empty indirection yields no secret")
	t.Setenv("BFF_TEST_SECRET_VALUE", "s3cret")
	testutil.Equal(t, SecretFromEnv("BFF_TEST_SECRET_VALUE"), "s3cret", "secret read from the environment")
	testutil.Equal(t, SecretFromEnv("BFF_TEST_SECRET_ABSENT"), "", "an unset variable yields no secret")
}

// ---------------------------------------------------------------------------
// Defaults and validation
// ---------------------------------------------------------------------------

// TestDefault_IsValid verifies REQ-CFG-007: the built-in defaults are complete
// and self-consistent, so the service starts with no configuration file.
func TestDefault_IsValid(t *testing.T) {
	t.Parallel()

	cfg := Default()
	testutil.NoError(t, Validate(&cfg), "the built-in defaults must pass validation")

	loaded, err := LoadWithEnv("", nil)
	testutil.NoError(t, err, "loading with no file and no environment must succeed")
	testutil.Equal(t, loaded.Server.HTTPAddr, cfg.Server.HTTPAddr, "loaded defaults match Default()")
	testutil.Equal(t, len(loaded.Routing.RequestTypes), len(cfg.Routing.RequestTypes), "every request type is present")
}

// TestValidate_Rules verifies REQ-CFG-003: every configuration invariant is
// checked, and each violation names itself in the joined error.
func TestValidate_Rules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"http addr", func(c *Config) { c.Server.HTTPAddr = "" }, "server.http_addr must be set"},
		{"request timeout", func(c *Config) { c.Server.RequestTimeout = 0 }, "server.request_timeout must be > 0"},
		{"max body bytes", func(c *Config) { c.Server.MaxBodyBytes = 0 }, "server.max_body_bytes must be > 0"},
		{"shutdown grace", func(c *Config) { c.Server.ShutdownGrace = 0 }, "server.shutdown_grace must be > 0"},

		{"jwt issuer", func(c *Config) { c.Security.JWT.Issuer = "" }, "security.jwt.issuer must be set"},
		{"jwt audience", func(c *Config) { c.Security.JWT.Audience = "" }, "security.jwt.audience must be set"},
		{"no key material", func(c *Config) {
			c.Security.JWT.JWKSURL = ""
			c.Security.JWT.HS256SecretEnv = ""
		}, "one of jwks_url or hs256_secret_env"},
		{"tenant claim", func(c *Config) { c.Security.JWT.TenantClaim = "" }, "security.jwt.tenant_claim must be set"},
		{"insecure outside local", func(c *Config) {
			c.Security.AllowInsecureNoAuth = true
			c.Observability.Environment = "production"
		}, "allow_insecure_no_auth is only permitted"},
		{"rate limit rps", func(c *Config) { c.Security.RateLimit.RPS = 0 }, "security.rate_limit.rps must be > 0"},
		{"rate limit burst", func(c *Config) { c.Security.RateLimit.Burst = 1 }, "burst (1) should be >= rps"},

		{"operational addr", func(c *Config) { c.Sources.Operational.Addr = "" }, "sources.operational.addr must be set"},
		{"execution base url", func(c *Config) { c.Sources.Execution.BaseURL = "" }, "sources.execution.base_url must be set"},
		{"execution base url scheme", func(c *Config) { c.Sources.Execution.BaseURL = "eds.internal:9102" }, "must be an http(s) URL"},

		{"call timeout", func(c *Config) { c.Sources.Operational.CallTimeout = 0 }, "sources.operational.call_timeout must be > 0"},
		{"retry attempts low", func(c *Config) { c.Sources.Operational.Retry.MaxAttempts = 0 }, "retry.max_attempts must be >= 1"},
		{"retry attempts high", func(c *Config) { c.Sources.Operational.Retry.MaxAttempts = 6 }, "retry.max_attempts must be <= 5"},
		{"retry base backoff", func(c *Config) { c.Sources.Operational.Retry.BaseBackoff = 0 }, "retry.base_backoff must be > 0"},
		{"retry max backoff", func(c *Config) { c.Sources.Operational.Retry.MaxBackoff = Duration(time.Millisecond) }, "retry.max_backoff must be >= base_backoff"},
		{"jitter above one", func(c *Config) { c.Sources.Operational.Retry.JitterFraction = 1.5 }, "retry.jitter_fraction must be within [0,1]"},
		{"jitter zero", func(c *Config) { c.Sources.Operational.Retry.JitterFraction = 0 }, "synchronised retries cause thundering herds"},
		{"budget ratio", func(c *Config) { c.Sources.Operational.Retry.BudgetRatio = 0 }, "retry.budget_ratio must be within (0,1]"},

		{"breaker threshold", func(c *Config) { c.Sources.Operational.Breaker.FailureThreshold = 0 }, "breaker.failure_threshold must be within (0,1]"},
		{"breaker minimum", func(c *Config) { c.Sources.Operational.Breaker.MinimumRequests = 0 }, "breaker.minimum_requests must be >= 1"},
		{"breaker window", func(c *Config) { c.Sources.Operational.Breaker.Window = 0 }, "breaker.window must be > 0"},
		{"breaker open timeout", func(c *Config) { c.Sources.Operational.Breaker.OpenTimeout = 0 }, "breaker.open_timeout must be > 0"},
		{"breaker half open calls", func(c *Config) { c.Sources.Operational.Breaker.HalfOpenMaxCalls = 0 }, "breaker.half_open_max_calls must be >= 1"},
		{"breaker half open successes", func(c *Config) { c.Sources.Operational.Breaker.HalfOpenSuccesses = 0 }, "breaker.half_open_successes must be >= 1"},

		{"bulkhead concurrency", func(c *Config) { c.Sources.Execution.Bulkhead.MaxConcurrent = 0 }, "sources.execution.bulkhead.max_concurrent must be >= 1"},
		{"bulkhead acquire timeout", func(c *Config) { c.Sources.Execution.Bulkhead.AcquireTimeout = Duration(-1) }, "bulkhead.acquire_timeout must be >= 0"},

		{"probe timeout zero", func(c *Config) { c.Sources.Operational.ProbeTimeout = 0 }, "freshness_probe_timeout must be > 0"},

		{"cache backend", func(c *Config) { c.Cache.Backend = "sqlite" }, "cache.backend must be one of"},
		{"redis addr", func(c *Config) {
			c.Cache.Backend = "redis"
			c.Cache.Redis.Addr = ""
		}, "cache.redis.addr must be set"},
		{"key prefix", func(c *Config) { c.Cache.KeyPrefix = "" }, "cache.key_prefix must be set"},
		{"stale grace", func(c *Config) { c.Cache.StaleGrace = Duration(-1) }, "cache.stale_grace must be >= 0"},
		{"early refresh ratio", func(c *Config) { c.Cache.Stampede.EarlyRefreshRatio = 1.4 }, "early_refresh_ratio must be within [0,1]"},

		{"unknown freshness source", func(c *Config) { c.Routing.Defaults.OnUnknownFreshness = "guess" }, "on_unknown_freshness must be operational|execution|none"},
		{"clock skew", func(c *Config) { c.Routing.Defaults.ClockSkewTolerance = Duration(-1) }, "clock_skew_tolerance must be >= 0"},
		{"in flight timeout", func(c *Config) { c.Routing.Defaults.InFlightLookupTimeout = 0 }, "in_flight_lookup_timeout must be > 0"},
		{"no request types", func(c *Config) { c.Routing.RequestTypes = nil }, "must define at least one request type"},

		{"rule preferred source", func(c *Config) {
			mutateRule(c, "resource_status", func(r *RoutingRule) { r.PreferredSource = "magic" })
		},
			"preferred_source must be operational|execution|both"},
		{"rule fallback", func(c *Config) { mutateRule(c, "resource_status", func(r *RoutingRule) { r.Fallback = "magic" }) },
			"fallback must be operational|execution|none"},
		{"rule negative ttl", func(c *Config) { mutateRule(c, "resource_status", func(r *RoutingRule) { r.TTL = Duration(-1) }) },
			"ttl must be >= 0"},
		{"rule negative cache ttl", func(c *Config) { mutateRule(c, "resource_status", func(r *RoutingRule) { r.CacheTTL = Duration(-1) }) },
			"cache_ttl must be >= 0"},
		{"rule allow stale without ceiling", func(c *Config) {
			mutateRule(c, "resource_status", func(r *RoutingRule) { r.AllowStale = true; r.MaxStale = 0 })
		}, "allow_stale requires max_stale > 0"},
		{"rule max stale below ttl", func(c *Config) {
			mutateRule(c, "resource_status", func(r *RoutingRule) { r.MaxStale = Duration(time.Second) })
		}, "must be >= ttl"},
		{"rule consistency", func(c *Config) {
			mutateRule(c, "resource_status", func(r *RoutingRule) { r.Consistency = "immediate" })
		},
			"consistency must be strong|bounded|eventual"},
		{"rule strong plus stale", func(c *Config) {
			mutateRule(c, "resource_status", func(r *RoutingRule) { r.Consistency = "strong"; r.AllowStale = true })
		}, "consistency=strong is incompatible with allow_stale=true"},
		{"rule required source", func(c *Config) {
			mutateRule(c, "resource_details", func(r *RoutingRule) { r.RequiredSources = map[string]bool{"billing": true} })
		}, "required_sources: unknown source"},
		{"rule per source timeout name", func(c *Config) {
			mutateRule(c, "resource_details", func(r *RoutingRule) {
				r.PerSourceTimeout = map[string]Duration{"billing": Duration(time.Second)}
			})
		}, "per_source_timeout: unknown source"},
		{"rule per source timeout value", func(c *Config) {
			mutateRule(c, "resource_details", func(r *RoutingRule) {
				r.PerSourceTimeout = map[string]Duration{"operational": 0}
			})
		}, "per_source_timeout.operational must be > 0"},

		{"precedence empty list", func(c *Config) { c.Precedence.Fields["status"] = nil }, "must list at least one source"},
		{"precedence unknown source", func(c *Config) { c.Precedence.Fields["status"] = []string{"billing"} }, "unknown source \"billing\""},
		{"precedence override unknown field", func(c *Config) {
			c.Precedence.ExecutionOverridesWhenRunning = []string{"nonexistent"}
		}, "is not a field in precedence.fields"},

		{"tenant empty id", func(c *Config) { c.Tenants[""] = Override{} }, "tenants: empty tenant id"},
		{"tenant unknown request type", func(c *Config) {
			c.Tenants["acme"] = Override{Routing: &RoutingConfig{
				RequestTypes: map[string]RoutingRule{"resource_vibes": {TTL: Duration(time.Second)}},
			}}
		}, "unknown request type"},
		{"tenant bad partial rule", func(c *Config) {
			c.Tenants["acme"] = Override{Routing: &RoutingConfig{
				RequestTypes: map[string]RoutingRule{"resource_status": {PreferredSource: "magic"}},
			}}
		}, "tenants.acme.routing.request_types.resource_status.preferred_source"},

		{"trace ratio", func(c *Config) { c.Observability.TraceSampleRatio = 2 }, "trace_sample_ratio must be within [0,1]"},
		{"log level", func(c *Config) { c.Observability.Log.Level = "chatty" }, "log.level must be debug|info|warn|error"},
		{"log format", func(c *Config) { c.Observability.Log.Format = "xml" }, "log.format must be json|text"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tc.mut(&cfg)
			err := Validate(&cfg)
			testutil.Error(t, err, "the mutated configuration must be refused")
			testutil.True(t, strings.Contains(err.Error(), tc.want),
				"error should mention %q\n  got: %v", tc.want, err)
		})
	}
}

// TestValidate_ReportsEveryProblemAtOnce verifies REQ-CFG-003: an operator
// fixing configuration is told about every problem, not just the first one.
func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.HTTPAddr = ""
	cfg.Cache.KeyPrefix = ""
	cfg.Observability.Log.Level = "chatty"
	cfg.Sources.Operational.Addr = ""

	err := Validate(&cfg)
	testutil.Error(t, err, "four broken keys must be refused")

	for _, want := range []string{
		"server.http_addr must be set",
		"cache.key_prefix must be set",
		"log.level must be debug|info|warn|error",
		"sources.operational.addr must be set",
	} {
		testutil.True(t, strings.Contains(err.Error(), want),
			"every problem must be reported; %q missing from: %v", want, err)
	}
}

// TestValidate_CacheTTLNeverExceedsFreshnessTTL verifies REQ-CFG-003 and
// REQ-CACHE-001: the response cache must never be able to hand back data that
// the source freshness policy already calls stale.
func TestValidate_CacheTTLNeverExceedsFreshnessTTL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		ttl      time.Duration
		cacheTTL time.Duration
		wantErr  bool
	}{
		{name: "cache below ttl", ttl: 10 * time.Second, cacheTTL: 3 * time.Second},
		{name: "cache equals ttl", ttl: 10 * time.Second, cacheTTL: 10 * time.Second},
		{name: "cache above ttl", ttl: 10 * time.Second, cacheTTL: 11 * time.Second, wantErr: true},
		{name: "cache far above ttl", ttl: time.Second, cacheTTL: time.Hour, wantErr: true},
		// ttl == 0 means "always read live"; the cache_ttl invariant does not
		// apply because there is no freshness window to exceed.
		{name: "always-live ttl", ttl: 0, cacheTTL: 5 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			mutateRule(&cfg, "resource_status", func(r *RoutingRule) {
				r.TTL = Duration(tc.ttl)
				r.CacheTTL = Duration(tc.cacheTTL)
				r.MaxStale = Duration(24 * time.Hour)
			})
			err := Validate(&cfg)
			if !tc.wantErr {
				testutil.NoError(t, err, "ttl=%s cache_ttl=%s should be accepted", tc.ttl, tc.cacheTTL)
				return
			}
			testutil.Error(t, err, "ttl=%s cache_ttl=%s must be refused", tc.ttl, tc.cacheTTL)
			testutil.True(t, strings.Contains(err.Error(), "the cache would serve data the TTL policy calls stale"),
				"error should explain the TTL/cache conflict, got: %v", err)
		})
	}
}

// TestValidate_ProbeTimeoutShorterThanCallTimeout verifies REQ-CFG-003: the
// cheap freshness probe must be strictly cheaper than the full read, or it is
// not a probe.
func TestValidate_ProbeTimeoutShorterThanCallTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		probe   time.Duration
		call    time.Duration
		wantErr bool
	}{
		{name: "probe well below call", probe: 120 * time.Millisecond, call: 400 * time.Millisecond},
		{name: "probe equals call", probe: 400 * time.Millisecond, call: 400 * time.Millisecond, wantErr: true},
		{name: "probe exceeds call", probe: 900 * time.Millisecond, call: 400 * time.Millisecond, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Sources.Operational.ProbeTimeout = Duration(tc.probe)
			cfg.Sources.Operational.CallTimeout = Duration(tc.call)
			err := Validate(&cfg)
			if !tc.wantErr {
				testutil.NoError(t, err, "probe=%s call=%s should be accepted", tc.probe, tc.call)
				return
			}
			testutil.Error(t, err, "probe=%s call=%s must be refused", tc.probe, tc.call)
			testutil.True(t, strings.Contains(err.Error(), "the probe exists to be cheap"),
				"error should explain why, got: %v", err)
		})
	}
}

// TestValidate_TenantOverrideIsAPatch verifies REQ-MT-005: a tenant may change
// one field of a routing rule without restating the rest, but a field it does
// set is still validated.
func TestValidate_TenantOverrideIsAPatch(t *testing.T) {
	t.Parallel()

	t.Run("partial override is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Tenants = map[string]Override{
			"acme": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
				"resource_status": {TTL: Duration(45 * time.Second), MaxStale: Duration(5 * time.Minute)},
			}}},
		}
		testutil.NoError(t, Validate(&cfg), "a tenant setting only a TTL must be accepted")
	})

	t.Run("malformed override is refused", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Tenants = map[string]Override{
			"acme": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
				// max_stale below ttl, and cache_ttl above ttl: both are checked
				// even though preferred_source was never restated.
				"resource_status": {
					TTL:      Duration(60 * time.Second),
					CacheTTL: Duration(90 * time.Second),
					MaxStale: Duration(10 * time.Second),
				},
			}}},
		}
		err := Validate(&cfg)
		testutil.Error(t, err, "a malformed tenant patch must be refused")
		testutil.True(t, strings.Contains(err.Error(), "cache_ttl"), "cache_ttl problem reported: %v", err)
		testutil.True(t, strings.Contains(err.Error(), "max_stale"), "max_stale problem reported: %v", err)
	})

	t.Run("tenant strong consistency with stale is refused", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Tenants = map[string]Override{
			"acme": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
				"resource_status": {Consistency: "strong", AllowStale: true, MaxStale: Duration(time.Minute)},
			}}},
		}
		err := Validate(&cfg)
		testutil.Error(t, err, "strong consistency plus allow_stale must be refused for a tenant too")
		testutil.True(t, strings.Contains(err.Error(), "incompatible with allow_stale"), "got: %v", err)
	})
}

// ---------------------------------------------------------------------------
// Tenant resolution
// ---------------------------------------------------------------------------

// TestResolveRule verifies REQ-MT-005 and REQ-CFG-005: a tenant override is
// merged field-by-field, so a tenant that changes only the TTL keeps the base
// rule's preferred source, fallback and consistency.
func TestResolveRule(t *testing.T) {
	t.Parallel()

	base := Default()
	base.Tenants = map[string]Override{
		"ttl-only": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
			"resource_status": {TTL: Duration(90 * time.Second)},
		}}},
		"everything": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
			"resource_status": {
				PreferredSource:  "execution",
				Fallback:         "none",
				Consistency:      "eventual",
				TTL:              Duration(2 * time.Minute),
				CacheTTL:         Duration(30 * time.Second),
				MaxStale:         Duration(10 * time.Minute),
				MaxLatency:       Duration(900 * time.Millisecond),
				AllowStale:       false,
				RequiredSources:  map[string]bool{"execution": true},
				PerSourceTimeout: map[string]Duration{"execution": Duration(time.Second)},
				RequiredFields:   []string{"status"},
			},
		}}},
		"other-type-only": {Routing: &RoutingConfig{RequestTypes: map[string]RoutingRule{
			"resource_details": {TTL: Duration(time.Minute)},
		}}},
		"no-routing": {Cache: &CacheOverride{NegativeTTL: ptr(Duration(time.Second))}},
	}
	baseRule := base.Routing.RequestTypes["resource_status"]

	t.Run("unknown request type", func(t *testing.T) {
		t.Parallel()
		_, ok := base.ResolveRule("ttl-only", "resource_vibes")
		testutil.False(t, ok, "an unknown request type must not resolve")
	})

	t.Run("unknown tenant falls through to the base rule", func(t *testing.T) {
		t.Parallel()
		got, ok := base.ResolveRule("nobody", "resource_status")
		testutil.True(t, ok, "the base rule must resolve")
		testutil.Equal(t, got, baseRule, "an unknown tenant sees the base rule verbatim")
	})

	t.Run("tenant without a routing block", func(t *testing.T) {
		t.Parallel()
		got, _ := base.ResolveRule("no-routing", "resource_status")
		testutil.Equal(t, got, baseRule, "a tenant with no routing override sees the base rule")
	})

	t.Run("tenant overriding a different request type", func(t *testing.T) {
		t.Parallel()
		got, _ := base.ResolveRule("other-type-only", "resource_status")
		testutil.Equal(t, got, baseRule, "an override of another type must not leak")
	})

	t.Run("ttl-only override keeps the rest of the base rule", func(t *testing.T) {
		t.Parallel()
		got, ok := base.ResolveRule("ttl-only", "resource_status")
		testutil.True(t, ok, "the rule must resolve")
		testutil.Equal(t, got.TTL.D(), 90*time.Second, "the tenant's TTL wins")
		testutil.Equal(t, got.PreferredSource, baseRule.PreferredSource, "preferred_source is inherited")
		testutil.Equal(t, got.Fallback, baseRule.Fallback, "fallback is inherited")
		testutil.Equal(t, got.Consistency, baseRule.Consistency, "consistency is inherited")
		testutil.Equal(t, got.CacheTTL, baseRule.CacheTTL, "cache_ttl is inherited")
		testutil.Equal(t, got.MaxStale, baseRule.MaxStale, "max_stale is inherited")
		testutil.Equal(t, got.MaxLatency, baseRule.MaxLatency, "max_latency is inherited")
		testutil.Equal(t, got.AllowStale, baseRule.AllowStale,
			"allow_stale is only overridable alongside max_stale, so it is inherited here")
	})

	t.Run("full override replaces every field it sets", func(t *testing.T) {
		t.Parallel()
		got, _ := base.ResolveRule("everything", "resource_status")
		testutil.Equal(t, got.PreferredSource, "execution", "preferred_source")
		testutil.Equal(t, got.Fallback, "none", "fallback")
		testutil.Equal(t, got.Consistency, "eventual", "consistency")
		testutil.Equal(t, got.TTL.D(), 2*time.Minute, "ttl")
		testutil.Equal(t, got.CacheTTL.D(), 30*time.Second, "cache_ttl")
		testutil.Equal(t, got.MaxStale.D(), 10*time.Minute, "max_stale")
		testutil.Equal(t, got.MaxLatency.D(), 900*time.Millisecond, "max_latency")
		testutil.Equal(t, got.RequiredSources, map[string]bool{"execution": true}, "required_sources")
		testutil.Equal(t, got.PerSourceTimeout, map[string]Duration{"execution": Duration(time.Second)}, "per_source_timeout")
		testutil.Equal(t, got.RequiredFields, []string{"status"}, "required_fields")
		testutil.False(t, got.AllowStale, "allow_stale is overridable because max_stale was also set")
	})

	t.Run("resolution does not mutate the base rule", func(t *testing.T) {
		t.Parallel()
		_, _ = base.ResolveRule("everything", "resource_status")
		testutil.Equal(t, base.Routing.RequestTypes["resource_status"], baseRule,
			"resolving a tenant rule must leave the global table untouched")
	})
}

// TestResolveDefaults verifies REQ-MT-005: routing defaults follow the same
// field-by-field overlay, with the paired flags only honoured alongside the
// duration that makes them meaningful.
func TestResolveDefaults(t *testing.T) {
	t.Parallel()

	cfg := Default()
	globalDefaults := cfg.Routing.Defaults
	cfg.Tenants = map[string]Override{
		"partial": {Routing: &RoutingConfig{Defaults: RoutingDefaults{
			OnUnknownFreshness: "execution",
			ClockSkewTolerance: Duration(9 * time.Second),
		}}},
		"flags": {Routing: &RoutingConfig{Defaults: RoutingDefaults{
			MaxStale:                 Duration(time.Hour),
			AllowStale:               Bool(false),
			ProbeCacheTTL:            Duration(4 * time.Second),
			ProbeEnabled:             Bool(false),
			InFlightLookupTimeout:    Duration(50 * time.Millisecond),
			ResolveInFlightExecution: Bool(false),
		}}},
		// A tenant that turns one flag off and sets nothing else. With plain
		// bools this override was unreachable, because "false" and "absent"
		// were the same value.
		"flag-only": {Routing: &RoutingConfig{Defaults: RoutingDefaults{
			AllowStale: Bool(false),
		}}},
		"nothing": {},
	}

	t.Run("unknown tenant", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, cfg.ResolveDefaults("nobody"), globalDefaults, "unknown tenant sees global defaults")
	})

	t.Run("tenant without routing", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, cfg.ResolveDefaults("nothing"), globalDefaults, "no routing override means global defaults")
	})

	t.Run("partial override", func(t *testing.T) {
		t.Parallel()
		got := cfg.ResolveDefaults("partial")
		testutil.Equal(t, got.OnUnknownFreshness, "execution", "on_unknown_freshness overridden")
		testutil.Equal(t, got.ClockSkewTolerance.D(), 9*time.Second, "clock_skew_tolerance overridden")
		testutil.Equal(t, got.MaxStale, globalDefaults.MaxStale, "max_stale inherited")
		testutil.Equal(t, got.AllowStale, globalDefaults.AllowStale, "allow_stale inherited")
		testutil.Equal(t, got.ProbeEnabled, globalDefaults.ProbeEnabled, "probe_enabled inherited")
	})

	t.Run("flags and durations override independently", func(t *testing.T) {
		t.Parallel()
		got := cfg.ResolveDefaults("flags")
		testutil.Equal(t, got.MaxStale.D(), time.Hour, "max_stale overridden")
		testutil.False(t, got.StaleAllowed(), "allow_stale overridden")
		testutil.Equal(t, got.ProbeCacheTTL.D(), 4*time.Second, "probe_cache_ttl overridden")
		testutil.False(t, got.ProbesEnabled(), "probe_enabled overridden")
		testutil.Equal(t, got.InFlightLookupTimeout.D(), 50*time.Millisecond, "in_flight_lookup_timeout overridden")
		testutil.False(t, got.InFlightResolutionEnabled(), "resolve_in_flight_execution overridden")
	})

	t.Run("a flag can be turned off on its own", func(t *testing.T) {
		t.Parallel()
		got := cfg.ResolveDefaults("flag-only")
		testutil.False(t, got.StaleAllowed(),
			"a tenant must be able to forbid stale data without also restating a duration")
		testutil.Equal(t, got.MaxStale, globalDefaults.MaxStale, "and everything else is inherited")
		testutil.True(t, got.ProbesEnabled(), "including the flags it did not mention")
	})
}

// TestResolvePrecedence verifies REQ-MT-005 and REQ-PREC-001: precedence is a
// per-tenant policy, and a tenant that supplies only the override list keeps
// the global field table.
func TestResolvePrecedence(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tenants = map[string]Override{
		"full": {Precedence: &PrecedenceConfig{
			Fields:                        map[string][]string{"status": {"execution", "operational"}},
			ExecutionOverridesWhenRunning: []string{"status"},
			ConflictWarning:               false,
		}},
		"list-only": {Precedence: &PrecedenceConfig{
			ExecutionOverridesWhenRunning: []string{"subState"},
			ConflictWarning:               true,
		}},
		"none": {},
	}

	t.Run("unknown tenant", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, cfg.ResolvePrecedence("nobody"), cfg.Precedence, "unknown tenant sees the global policy")
	})

	t.Run("tenant without a precedence block", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, cfg.ResolvePrecedence("none"), cfg.Precedence, "no override means the global policy")
	})

	t.Run("full override", func(t *testing.T) {
		t.Parallel()
		got := cfg.ResolvePrecedence("full")
		testutil.Equal(t, got.Fields["status"], []string{"execution", "operational"}, "field order overridden")
		testutil.False(t, got.ConflictWarning, "conflict_warning overridden")
	})

	t.Run("override with no field table inherits the global table", func(t *testing.T) {
		t.Parallel()
		got := cfg.ResolvePrecedence("list-only")
		testutil.Equal(t, got.Fields, cfg.Precedence.Fields, "the global field table is inherited")
		testutil.Equal(t, got.ExecutionOverridesWhenRunning, []string{"subState"}, "the override list is the tenant's")
	})
}

// TestResolveRateLimit verifies REQ-MT-005 and REQ-RES-008: a tenant may carry
// its own admission policy, and everyone else shares the global one.
func TestResolveRateLimit(t *testing.T) {
	t.Parallel()

	cfg := Default()
	tenantLimit := RateLimitConfig{Enabled: true, RPS: 5, Burst: 10, PerTenant: true, MaxTenants: 100}
	cfg.Tenants = map[string]Override{
		"throttled":      {Security: &SecurityOverride{RateLimit: &tenantLimit}},
		"no-security":    {},
		"empty-security": {Security: &SecurityOverride{}},
	}

	testutil.Equal(t, cfg.ResolveRateLimit("throttled"), tenantLimit, "the tenant's own limit applies")
	testutil.Equal(t, cfg.ResolveRateLimit("nobody"), cfg.Security.RateLimit, "an unknown tenant shares the global limit")
	testutil.Equal(t, cfg.ResolveRateLimit("no-security"), cfg.Security.RateLimit, "no security override means the global limit")
	testutil.Equal(t, cfg.ResolveRateLimit("empty-security"), cfg.Security.RateLimit, "an empty security override means the global limit")
}

// TestRequestTypesAndRedacted verifies REQ-CFG-006 and REQ-SEC-008: the
// configured request types are introspectable and the loggable snapshot masks
// the Redis identity.
func TestRequestTypesAndRedacted(t *testing.T) {
	t.Parallel()

	cfg := Default()
	types := cfg.RequestTypes()
	testutil.Equal(t, len(types), len(cfg.Routing.RequestTypes), "every request type is listed")
	seen := map[string]bool{}
	for _, ty := range types {
		seen[ty] = true
	}
	for name := range cfg.Routing.RequestTypes {
		testutil.True(t, seen[name], "request type %q must be listed", name)
	}

	cfg.Cache.Redis.Username = "iam-role-arn"
	testutil.Equal(t, cfg.Redacted().Cache.Redis.Username, "***", "the Redis identity is masked")
	testutil.Equal(t, cfg.Cache.Redis.Username, "iam-role-arn", "Redacted must not mutate the original")

	cfg.Cache.Redis.Username = ""
	testutil.Equal(t, cfg.Redacted().Cache.Redis.Username, "", "an empty identity stays empty")
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

// TestProvider_Get verifies REQ-CFG-004: a fixed provider hands back exactly
// the configuration it was built with.
func TestProvider_Get(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.HTTPAddr = ":12345"
	p := NewProvider(cfg)

	got := p.Get()
	testutil.Equal(t, got.Server.HTTPAddr, ":12345", "Get returns the loaded configuration")
	testutil.True(t, p.Get() == got, "Get is a plain atomic load and returns the same snapshot")

	reloads, failures := p.Stats()
	testutil.Equal(t, reloads, int64(0), "a fixed provider has never reloaded")
	testutil.Equal(t, failures, int64(0), "a fixed provider has never failed")
}

// TestProvider_FileReloadPicksUpChanges verifies REQ-CFG-004: a reload swaps
// the snapshot atomically and notifies observers.
func TestProvider_FileReloadPicksUpChanges(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
routing:
  request_types:
    resource_status:
      ttl: "10s"
      cache_ttl: "1s"
`)

	p, err := NewFileProvider(path, 0, nil)
	testutil.NoError(t, err, "NewFileProvider")
	testutil.Equal(t, p.Get().Routing.RequestTypes["resource_status"].CacheTTL.D(), time.Second, "initial cache_ttl")

	var observed int
	var newTTL time.Duration
	p.OnChange(func(_, updated *Config) {
		observed++
		newTTL = updated.Routing.RequestTypes["resource_status"].CacheTTL.D()
	})

	rewriteConfig(t, path, `
routing:
  request_types:
    resource_status:
      ttl: "10s"
      cache_ttl: "7s"
`)
	testutil.NoError(t, p.Reload(), "Reload")

	testutil.Equal(t, p.Get().Routing.RequestTypes["resource_status"].CacheTTL.D(), 7*time.Second, "reloaded cache_ttl")
	testutil.Equal(t, observed, 1, "the change observer fired exactly once")
	testutil.Equal(t, newTTL, 7*time.Second, "the observer saw the new snapshot")

	reloads, failures := p.Stats()
	testutil.Equal(t, reloads, int64(1), "one successful reload")
	testutil.Equal(t, failures, int64(0), "no failures")
}

// TestProvider_InvalidReloadKeepsPreviousConfig verifies REQ-CFG-004: a typo in
// a ConfigMap must not take the service down. The previous snapshot stays in
// force and the failure is counted.
func TestProvider_InvalidReloadKeepsPreviousConfig(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
server:
  http_addr: ":7100"
routing:
  request_types:
    resource_status:
      ttl: "10s"
      cache_ttl: "2s"
`)

	p, err := NewFileProvider(path, 0, nil)
	testutil.NoError(t, err, "NewFileProvider")
	before := p.Get()
	testutil.Equal(t, before.Server.HTTPAddr, ":7100", "initial addr")

	var observed int
	p.OnChange(func(*Config, *Config) { observed++ })

	// cache_ttl above ttl is exactly the invariant the validator refuses.
	rewriteConfig(t, path, `
server:
  http_addr: ":7200"
routing:
  request_types:
    resource_status:
      ttl: "10s"
      cache_ttl: "60s"
`)

	err = p.Reload()
	testutil.Error(t, err, "an invalid reload must be refused")
	testutil.True(t, strings.Contains(err.Error(), "cache_ttl"),
		"the rejection should name the offending key, got: %v", err)

	after := p.Get()
	testutil.True(t, after == before, "the previous snapshot pointer must still be in force")
	testutil.Equal(t, after.Server.HTTPAddr, ":7100", "the previous configuration is unchanged")
	testutil.Equal(t, after.Routing.RequestTypes["resource_status"].CacheTTL.D(), 2*time.Second, "the previous cache_ttl stands")
	testutil.Equal(t, observed, 0, "observers must not fire for a rejected reload")

	reloads, failures := p.Stats()
	testutil.Equal(t, reloads, int64(0), "a rejected reload is not a reload")
	testutil.Equal(t, failures, int64(1), "the failure counter is incremented")

	// A subsequent valid reload still works: the provider is not poisoned.
	rewriteConfig(t, path, `
server:
  http_addr: ":7300"
routing:
  request_types:
    resource_status:
      ttl: "10s"
      cache_ttl: "3s"
`)
	testutil.NoError(t, p.Reload(), "a later valid reload must succeed")
	testutil.Equal(t, p.Get().Server.HTTPAddr, ":7300", "the good configuration is now in force")

	reloads, failures = p.Stats()
	testutil.Equal(t, reloads, int64(1), "one successful reload")
	testutil.Equal(t, failures, int64(1), "the earlier failure is still counted")
}

// TestProvider_ReloadUnreadableFile verifies REQ-CFG-004: a file that vanishes
// between reloads is a counted failure, not a panic or an empty configuration.
func TestProvider_ReloadUnreadableFile(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "server:\n  http_addr: \":7400\"\n")
	p, err := NewFileProvider(path, 0, nil)
	testutil.NoError(t, err, "NewFileProvider")

	testutil.NoError(t, os.Remove(path), "remove the config file")

	testutil.Error(t, p.Reload(), "a missing file must be a reload error")
	testutil.Equal(t, p.Get().Server.HTTPAddr, ":7400", "the previous configuration stays in force")
	_, failures := p.Stats()
	testutil.Equal(t, failures, int64(1), "the failure is counted")
}

// TestNewFileProvider_RejectsInvalidFile verifies REQ-CFG-003: the process
// refuses to start on invalid configuration.
func TestNewFileProvider_RejectsInvalidFile(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "observability:\n  log:\n    level: \"chatty\"\n")
	p, err := NewFileProvider(path, 0, nil)
	testutil.Error(t, err, "an invalid file must refuse to load")
	testutil.True(t, p == nil, "no provider is returned")

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err = NewFileProvider(missing, 0, nil)
	testutil.Error(t, err, "a missing file must refuse to load")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bff.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func rewriteConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
}

// mutateRule edits one routing rule in place; the map holds values, so it has
// to be read, changed and written back.
func mutateRule(c *Config, name string, fn func(*RoutingRule)) {
	r := c.Routing.RequestTypes[name]
	fn(&r)
	c.Routing.RequestTypes[name] = r
}

func ptr[T any](v T) *T { return &v }
