package resilience

import (
	"strconv"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// newTestLimiter builds a limiter driven by a manually advanced clock, so token
// refill is exercised without any test spending wall time. `now` and `lastGC`
// are package-private fields, which is why these are in-package tests.
func newTestLimiter(cfg config.RateLimitConfig) (*RateLimiter, *testutil.Clock) {
	clk := testutil.NewClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := NewRateLimiter(cfg)
	r.now = clk.Now
	r.lastGC = clk.Now()
	return r, clk
}

func perTenantCfg(rps, burst, maxTenants int) config.RateLimitConfig {
	return config.RateLimitConfig{
		Enabled: true, RPS: rps, Burst: burst, PerTenant: true, MaxTenants: maxTenants,
	}
}

// allowN calls Allow n times and reports how many were admitted.
func allowN(r *RateLimiter, tenant string, n int) int {
	admitted := 0
	for i := 0; i < n; i++ {
		if r.Allow(tenant) {
			admitted++
		}
	}
	return admitted
}

// TestRateLimiter_Disabled verifies REQ-RES-008: a disabled limiter admits
// everything and tracks nothing.
func TestRateLimiter_Disabled(t *testing.T) {
	t.Parallel()

	r, _ := newTestLimiter(config.RateLimitConfig{Enabled: false, RPS: 1, Burst: 1, PerTenant: true})
	testutil.Equal(t, allowN(r, "tenant-a", 1000), 1000, "a disabled limiter admits every request")
	testutil.Equal(t, r.Size(), 0, "and allocates no per-tenant state")
}

// TestRateLimiter_Burst verifies REQ-RES-008: the bucket admits a burst up to
// its configured size, then refuses until tokens refill at the configured rate.
func TestRateLimiter_Burst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		rps, burst int
		wantBurst  int
	}{
		{name: "burst above rps", rps: 2, burst: 5, wantBurst: 5},
		{name: "burst equals rps", rps: 3, burst: 3, wantBurst: 3},
		{name: "unset burst falls back to rps", rps: 4, burst: 0, wantBurst: 4},
		{name: "single token", rps: 1, burst: 1, wantBurst: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, clk := newTestLimiter(perTenantCfg(tc.rps, tc.burst, 0))

			testutil.Equal(t, allowN(r, "tenant-a", tc.wantBurst), tc.wantBurst,
				"the whole burst is admitted at once")
			testutil.False(t, r.Allow("tenant-a"), "the bucket is empty and the clock has not moved")
			testutil.Equal(t, allowN(r, "tenant-a", 20), 0, "and stays empty")

			// One second of refill yields exactly rps more tokens.
			clk.Advance(time.Second)
			testutil.Equal(t, allowN(r, "tenant-a", tc.rps+3), tc.rps,
				"one second refills exactly rps tokens")

			// A long idle period refills only up to the burst ceiling.
			clk.Advance(time.Hour)
			testutil.Equal(t, allowN(r, "tenant-a", tc.wantBurst+5), tc.wantBurst,
				"an idle bucket never accumulates beyond its burst")
		})
	}
}

// TestRateLimiter_PerTenantIsolation verifies REQ-MT-001 and REQ-RES-008: one
// tenant exhausting its bucket must have no effect on any other tenant.
func TestRateLimiter_PerTenantIsolation(t *testing.T) {
	t.Parallel()

	r, clk := newTestLimiter(perTenantCfg(2, 3, 0))

	testutil.Equal(t, allowN(r, "tenant-a", 3), 3, "tenant A spends its whole burst")
	testutil.False(t, r.Allow("tenant-a"), "tenant A is now throttled")

	testutil.Equal(t, allowN(r, "tenant-b", 3), 3, "tenant B is untouched by tenant A's spending")
	testutil.False(t, r.Allow("tenant-b"), "tenant B is throttled only by its own usage")
	testutil.False(t, r.Allow("tenant-a"), "and tenant A is still throttled")

	testutil.Equal(t, allowN(r, "tenant-c", 3), 3, "a third tenant is likewise unaffected")
	testutil.Equal(t, r.Size(), 3, "one limiter per tenant")

	// Refill is per tenant as well.
	clk.Advance(time.Second)
	testutil.Equal(t, allowN(r, "tenant-a", 5), 2, "tenant A refills at its own rate")
	testutil.Equal(t, allowN(r, "tenant-b", 5), 2, "and so does tenant B, independently")
}

// TestRateLimiter_GlobalMode verifies REQ-RES-008: with per_tenant off, every
// caller shares one bucket, and an unauthenticated caller with no tenant shares
// it too.
func TestRateLimiter_GlobalMode(t *testing.T) {
	t.Parallel()

	t.Run("per_tenant off", func(t *testing.T) {
		t.Parallel()
		cfg := perTenantCfg(1, 4, 0)
		cfg.PerTenant = false
		r, _ := newTestLimiter(cfg)

		testutil.True(t, r.Allow("tenant-a"), "1 of 4")
		testutil.True(t, r.Allow("tenant-b"), "2 of 4")
		testutil.True(t, r.Allow("tenant-c"), "3 of 4")
		testutil.True(t, r.Allow(""), "4 of 4")
		testutil.False(t, r.Allow("tenant-d"), "every caller shares one bucket")
		testutil.Equal(t, r.Size(), 1, "exactly one limiter is allocated")
	})

	t.Run("per_tenant on with no tenant id", func(t *testing.T) {
		t.Parallel()
		r, _ := newTestLimiter(perTenantCfg(1, 2, 0))

		testutil.Equal(t, allowN(r, "", 2), 2, "the anonymous bucket admits its burst")
		testutil.False(t, r.Allow(""), "and is then exhausted")
		testutil.True(t, r.Allow("tenant-a"), "an identified tenant has its own bucket")
		testutil.Equal(t, r.Size(), 2, "the anonymous bucket and the tenant's bucket")
	})
}

// TestRateLimiter_MaxTenantsBound verifies REQ-RES-008 and REQ-SEC-010: the
// per-tenant map is bounded, so a hostile or buggy issuer cannot grow it without
// limit. When the bound is reached the request is admitted rather than an active
// tenant being evicted; the bulkheads and server concurrency limits are the
// backstop.
func TestRateLimiter_MaxTenantsBound(t *testing.T) {
	t.Parallel()

	const maxTenants = 4
	r, _ := newTestLimiter(perTenantCfg(1, 1, maxTenants))

	for i := 0; i < maxTenants; i++ {
		testutil.True(t, r.Allow("tenant-"+strconv.Itoa(i)), "tenant %d gets its own bucket", i)
	}
	testutil.Equal(t, r.Size(), maxTenants, "the map holds exactly max_tenants limiters")

	// Every further tenant id is admitted without allocating anything.
	for i := 0; i < 500; i++ {
		testutil.True(t, r.Allow("attacker-"+strconv.Itoa(i)),
			"a tenant beyond the bound is admitted rather than tracked")
	}
	testutil.Equal(t, r.Size(), maxTenants, "the map has not grown")

	// The tenants that do have buckets are still limited normally.
	testutil.False(t, r.Allow("tenant-0"), "an established tenant is still throttled")

	t.Run("zero means unbounded", func(t *testing.T) {
		t.Parallel()
		u, _ := newTestLimiter(perTenantCfg(1, 1, 0))
		for i := 0; i < 50; i++ {
			u.Allow("tenant-" + strconv.Itoa(i))
		}
		testutil.Equal(t, u.Size(), 50, "max_tenants of zero imposes no bound")
	})
}

// TestRateLimiter_IdleEviction verifies REQ-RES-008: limiters for tenants that
// have gone quiet are evicted, but only after a long idle period. Evicting
// eagerly would reset a tenant's bucket between bursts and effectively double
// its allowance.
func TestRateLimiter_IdleEviction(t *testing.T) {
	t.Parallel()

	t.Run("an idle tenant is evicted when a new one arrives", func(t *testing.T) {
		t.Parallel()
		r, clk := newTestLimiter(perTenantCfg(1, 1, 0))

		testutil.True(t, r.Allow("idle"), "the idle tenant's first and only request")
		testutil.Equal(t, r.Size(), 1, "one limiter")

		clk.Advance(idleEviction + time.Minute)

		// Creating a limiter is the only path that runs the collector.
		testutil.True(t, r.Allow("newcomer"), "the newcomer is admitted")
		testutil.Equal(t, r.Size(), 1, "the idle tenant was evicted, leaving only the newcomer")
	})

	t.Run("a recently seen tenant survives the sweep", func(t *testing.T) {
		t.Parallel()
		r, clk := newTestLimiter(perTenantCfg(10, 10, 0))

		testutil.True(t, r.Allow("busy"), "the busy tenant appears")
		clk.Advance(idleEviction - time.Minute)
		testutil.True(t, r.Allow("busy"), "and is seen again just inside the idle window")

		clk.Advance(2 * time.Minute)
		testutil.True(t, r.Allow("newcomer"), "a newcomer triggers the sweep")
		testutil.Equal(t, r.Size(), 2, "the recently seen tenant is kept")
	})

	t.Run("the sweep is rate limited to once a minute", func(t *testing.T) {
		t.Parallel()
		r, clk := newTestLimiter(perTenantCfg(1, 1, 0))

		testutil.True(t, r.Allow("idle"), "the idle tenant appears")
		clk.Advance(idleEviction + time.Minute)

		// The first newcomer runs the sweep and evicts the idle tenant.
		testutil.True(t, r.Allow("first"), "first newcomer")
		testutil.Equal(t, r.Size(), 1, "the sweep ran")

		// A second newcomer arrives immediately: the sweep is skipped, so
		// "first" survives even though nothing has aged out anyway.
		testutil.True(t, r.Allow("second"), "second newcomer")
		testutil.Equal(t, r.Size(), 2, "no sweep runs within a minute of the last one")
	})

	t.Run("eviction restores a full bucket", func(t *testing.T) {
		t.Parallel()
		r, clk := newTestLimiter(perTenantCfg(1, 2, 0))

		testutil.Equal(t, allowN(r, "sporadic", 2), 2, "the tenant spends its burst")
		testutil.False(t, r.Allow("sporadic"), "and is throttled")

		clk.Advance(idleEviction + time.Minute)
		testutil.True(t, r.Allow("trigger"), "a newcomer sweeps the map")
		testutil.Equal(t, allowN(r, "sporadic", 3), 2,
			"the returning tenant gets a fresh bucket, which after this long idle "+
				"period is what a surviving bucket would have refilled to anyway")
	})
}

// TestRateLimiter_SizeReflectsTrackedTenants verifies the accessor the metrics
// and tests rely on (REQ-RES-008).
func TestRateLimiter_SizeReflectsTrackedTenants(t *testing.T) {
	t.Parallel()

	r, _ := newTestLimiter(perTenantCfg(5, 5, 0))
	testutil.Equal(t, r.Size(), 0, "a fresh limiter tracks nothing")

	r.Allow("tenant-a")
	testutil.Equal(t, r.Size(), 1, "one tenant")
	r.Allow("tenant-a")
	testutil.Equal(t, r.Size(), 1, "a repeat caller does not allocate again")
	r.Allow("tenant-b")
	testutil.Equal(t, r.Size(), 2, "two tenants")
}
