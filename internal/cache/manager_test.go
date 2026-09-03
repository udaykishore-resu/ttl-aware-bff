package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// recordingHooks counts the events the manager reports for metrics, so a test
// can assert that a fail-open path stayed observable rather than silent.
type recordingHooks struct {
	mu     sync.Mutex
	hits   int
	misses int
	errOps []string
}

func (h *recordingHooks) hooks() Hooks {
	return Hooks{
		OnHit: func(string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.hits++
		},
		OnMiss: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.misses++
		},
		OnError: func(op string, _ error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.errOps = append(h.errOps, op)
		},
	}
}

func (h *recordingHooks) errorOps() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.errOps...)
}

// managerFixture builds a manager over an in-process cache on a manual clock.
//
// Stampede protection is off by default: the early-refresh path spawns a
// background reload, and a test asserting "the loader was called once" must not
// race a goroutine that is entitled to call it again.
func managerFixture(mutate func(*config.CacheConfig)) (*Manager, *Memory, *testutil.Clock, *recordingHooks) {
	cfg := config.Default().Cache
	cfg.Stampede = config.StampedeConfig{}
	if mutate != nil {
		mutate(&cfg)
	}
	clk := testutil.NewClock(time.Now())
	mem := NewMemory(256).WithClock(clk.Now)
	h := &recordingHooks{}
	// The manager and the backing store share one clock: an entry stamped by
	// one and aged by another is meaningless.
	return NewManager(mem, cfg, h.hooks()).WithClock(clk.Now), mem, clk, h
}

// loaded builds what a loader hands back: an entry with no StoredAt, which the
// manager stamps as it stores. Leaving it unset is what a real loader does, and
// it is what makes the entry's age the cache's business rather than the
// loader's.
func loaded(rule string) *Entry {
	return &Entry{Payload: []byte(rule), RoutingRule: rule}
}

// countingLoader returns a loader and a call counter.
func countingLoader(rule string) (Loader, *atomic.Int64) {
	var calls atomic.Int64
	return func(context.Context) (*Entry, error) {
		calls.Add(1)
		return loaded(rule), nil
	}, &calls
}

// TestGetOrLoad_MissLoadsAndStores verifies REQ-CACHE-001: a miss consults the
// loader once and populates the cache, and the next request for the same key is
// served without touching a source.
func TestGetOrLoad_MissLoadsAndStores(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, hooks := managerFixture(nil)
	k := key("acme", "resource_status", "R1").String()
	load, calls := countingLoader("ttl.fresh")

	res, err := m.GetOrLoad(ctx, k, 30*time.Second, load)
	testutil.NoError(t, err, "first lookup")
	testutil.Equal(t, calls.Load(), int64(1), "the loader ran once")
	testutil.False(t, res.Hit, "the first lookup is a miss")
	testutil.Equal(t, res.Entry.RoutingRule, "ttl.fresh", "the loaded value is returned")

	res, err = m.GetOrLoad(ctx, k, 30*time.Second, load)
	testutil.NoError(t, err, "second lookup")
	testutil.Equal(t, calls.Load(), int64(1), "the second lookup was served from cache")
	testutil.True(t, res.Hit, "and is reported as a hit")
	testutil.Equal(t, res.Layer, "L1", "served by the in-process tier")

	stored, ok := m.Get(ctx, k)
	testutil.True(t, ok, "the entry is retrievable on its own")
	testutil.Equal(t, stored.Entry.CacheTTL, 30*time.Second,
		"the entry records the logical lifetime it was written with")
	testutil.Equal(t, stored.Entry.SchemaVersion, EntrySchemaVersion,
		"and the schema version, so a later build treats it as a miss rather than mis-decoding it")
	testutil.Equal(t, hooks.hits, 2, "both cache hits were reported for metrics")
	// The cold lookup checks the cache twice -- once before entering
	// singleflight and once inside it, in case another goroutine filled the key
	// while this one queued -- so a miss may be reported more than once per
	// request.
	testutil.True(t, hooks.misses >= 1, "the miss was reported for metrics, got %d", hooks.misses)
}

// TestGetOrLoad_ZeroTTLDoesNotStore verifies REQ-CACHE-001: cache lifetime is
// configuration, and `cache_ttl: 0s` is how a request type opts out. Execution
// history is configured that way because a paged history answer is not worth
// re-serving, and it must opt out without a special code path.
func TestGetOrLoad_ZeroTTLDoesNotStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)
	k := key("acme", "execution_history", "R1").String()

	for _, ttl := range []time.Duration{0, -time.Second} {
		load, calls := countingLoader("history")

		res, err := m.GetOrLoad(ctx, k, ttl, load)
		testutil.NoError(t, err, "lookup with ttl %s", ttl)
		testutil.Equal(t, res.Entry.RoutingRule, "history", "the loader's value is returned")
		testutil.Equal(t, res.Layer, "NONE", "and it was not served by any cache tier")

		_, ok := m.Get(ctx, k)
		testutil.False(t, ok, "nothing was stored for ttl %s", ttl)

		_, err = m.GetOrLoad(ctx, k, ttl, load)
		testutil.NoError(t, err, "second lookup with ttl %s", ttl)
		testutil.Equal(t, calls.Load(), int64(2), "every request loads afresh for ttl %s", ttl)
	}
}

// TestGetOrLoad_SingleflightCollapsesConcurrentMisses verifies REQ-EDGE-012 and
// REQ-CACHE-004: N concurrent identical misses produce one upstream read. This
// is what stops one popular resource from turning a cold cache into N source
// calls, which is how a cache miss becomes an outage.
func TestGetOrLoad_SingleflightCollapsesConcurrentMisses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)
	k := key("acme", "resource_read", "R1").String()

	const goroutines = 50
	var (
		calls    atomic.Int64
		entered  = make(chan struct{})
		release  = make(chan struct{})
		once     sync.Once
		launched sync.WaitGroup
		done     sync.WaitGroup
	)
	launched.Add(goroutines)
	done.Add(goroutines)

	// The loader blocks until every caller has had a chance to arrive, so the
	// misses really are concurrent rather than merely consecutive.
	load := func(context.Context) (*Entry, error) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		return loaded("loaded-once"), nil
	}

	results := make([]*Entry, goroutines)
	errsOut := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer done.Done()
			launched.Done()
			res, err := m.GetOrLoad(ctx, k, time.Minute, load)
			results[i], errsOut[i] = res.Entry, err
		}(i)
	}

	launched.Wait()
	<-entered
	close(release)
	done.Wait()

	testutil.Equal(t, calls.Load(), int64(1),
		"fifty concurrent misses for one key must produce exactly one source read")
	for i := 0; i < goroutines; i++ {
		testutil.NoError(t, errsOut[i], "caller %d", i)
		testutil.Equal(t, results[i].RoutingRule, "loaded-once", "caller %d received the shared answer", i)
	}
}

// TestGetOrLoad_LoaderErrorIsNotCached verifies REQ-CACHE-005: only a confirmed
// NOT_FOUND is remembered. Caching a transient upstream failure would extend a
// blip into an outage that outlives its cause.
func TestGetOrLoad_LoaderErrorIsNotCached(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)
	k := key("acme", "resource_status", "R1").String()

	boom := errs.New(errs.CodeUpstreamUnavailable, "source is down")
	res, err := m.GetOrLoad(ctx, k, time.Minute, func(context.Context) (*Entry, error) {
		return nil, boom
	})
	testutil.Error(t, err, "the loader's failure reaches the caller")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "the taxonomy code survives the cache layer")
	testutil.True(t, res.Entry == nil, "and no entry is produced")

	_, ok := m.Get(ctx, k)
	testutil.False(t, ok, "nothing was cached")

	_, _, staleOK := m.GetStale(ctx, k)
	testutil.False(t, staleOK, "not even as a stale-serve candidate")
}

// TestSetNegative verifies REQ-CACHE-005: a confirmed "not found" is remembered
// for the configured negative TTL, because a UI polling a resource that does
// not exist would otherwise hammer both sources indefinitely.
func TestSetNegative(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)
	k := key("acme", "resource_status", "ghost").String()

	m.SetNegative(ctx, k)

	res, ok := m.Get(ctx, k)
	testutil.True(t, ok, "the negative entry is a hit")
	testutil.True(t, res.Entry.Negative, "and is marked as a cached absence, not as data")
	testutil.Equal(t, res.Entry.CacheTTL, config.Default().Cache.NegativeTTL.D(),
		"the negative TTL is configuration, not a constant")

	t.Run("a negative entry is never a stale-serve candidate", func(t *testing.T) {
		t.Parallel()
		// The degradation ladder serves old data when every source is down;
		// serving a remembered 404 there would be worse than the outage.
		_, _, staleOK := m.GetStale(ctx, k)
		testutil.False(t, staleOK, "GetStale must not resurrect a negative entry")
	})

	t.Run("negative caching can be switched off", func(t *testing.T) {
		t.Parallel()
		off, _, _, _ := managerFixture(func(c *config.CacheConfig) { c.NegativeTTL = 0 })
		off.SetNegative(ctx, k)
		_, ok := off.Get(ctx, k)
		testutil.False(t, ok, "with a zero negative TTL nothing is remembered")
	})
}

// TestInvalidate verifies REQ-CACHE-002: named keys can be dropped, which is
// what a write elsewhere in the platform triggers.
func TestInvalidate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)

	keys := ResourceKeysForTypes(m.KeyPrefix(), "acme", "R1", []string{"resource_status", "resource_read"})
	other := key("acme", "resource_status", "R2").String()
	for _, k := range append(append([]string{}, keys...), other) {
		load, _ := countingLoader("x")
		_, err := m.GetOrLoad(ctx, k, time.Minute, load)
		testutil.NoError(t, err, "seed %s", k)
	}

	m.Invalidate(ctx, keys...)

	for _, k := range keys {
		_, ok := m.Get(ctx, k)
		testutil.False(t, ok, "%s was invalidated", k)
	}
	_, ok := m.Get(ctx, other)
	testutil.True(t, ok, "an unrelated resource keeps its entry")
	testutil.Equal(t, m.KeyPrefix(), config.Default().Cache.KeyPrefix, "the configured prefix is exposed")
}

// TestInvalidateTenant verifies REQ-MT-005: tenant-scoped invalidation reaches
// every entry of that tenant and nothing belonging to another.
func TestInvalidateTenant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _, _, _ := managerFixture(nil)

	acme := []string{
		key("acme", "resource_status", "R1").String(),
		key("acme", "resource_read", "R2").String(),
	}
	globex := key("globex", "resource_status", "R1").String()
	for _, k := range append(append([]string{}, acme...), globex) {
		load, _ := countingLoader("x")
		_, err := m.GetOrLoad(ctx, k, time.Minute, load)
		testutil.NoError(t, err, "seed %s", k)
	}

	testutil.NoError(t, m.InvalidateTenant(ctx, "acme"), "invalidate tenant")

	for _, k := range acme {
		_, ok := m.Get(ctx, k)
		testutil.False(t, ok, "%s belongs to the invalidated tenant", k)
	}
	_, ok := m.Get(ctx, globex)
	testutil.True(t, ok, "another tenant's entries are untouched")
}

// TestGetStale_LogicalAndPhysicalLifetimesDiffer verifies REQ-RES-007 and
// REQ-EDGE-005: an entry is retained past the TTL the response envelope
// enforces, so that when every source is down an old, clearly labelled answer
// can still be served. The split is the whole mechanism, so both halves are
// asserted: the ordinary read path cannot see the expired entry, and the
// explicit stale-serve path can -- until the grace period runs out too.
func TestGetStale_LogicalAndPhysicalLifetimesDiffer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const ttl = 30 * time.Second
	const grace = 60 * time.Second
	m, _, clk, _ := managerFixture(func(c *config.CacheConfig) {
		c.StaleGrace = config.Duration(grace)
	})
	k := key("acme", "resource_status", "R1").String()

	_, err := m.GetOrLoad(ctx, k, ttl, func(context.Context) (*Entry, error) { return loaded("ttl.stale"), nil })
	testutil.NoError(t, err, "seed")

	// Age the entry past its cache TTL but well inside the grace period.
	clk.Advance(ttl + time.Second)

	_, ok := m.Get(ctx, k)
	testutil.False(t, ok, "past its cache TTL the entry is a miss on the normal read path")

	got, layer, ok := m.GetStale(ctx, k)
	testutil.True(t, ok, "but the stale-serve path can still reach it")
	testutil.Equal(t, got.RoutingRule, "ttl.stale", "and it is the entry that was written")
	testutil.Equal(t, layer, "L1", "reported with the tier that answered")

	t.Run("past the grace period it is gone for good", func(t *testing.T) {
		clk.Advance(grace)
		_, _, ok := m.GetStale(ctx, k)
		testutil.False(t, ok,
			"the physical lifetime is the cache TTL plus the configured grace, and nothing outlives it")
	})
}

// brokenCache fails every operation, standing in for a Redis that is down.
type brokenCache struct{ err error }

func (b *brokenCache) Get(context.Context, string) (*Entry, error) { return nil, b.err }
func (b *brokenCache) Set(context.Context, string, *Entry, time.Duration) error {
	return b.err
}
func (b *brokenCache) Delete(context.Context, string) error       { return b.err }
func (b *brokenCache) DeletePrefix(context.Context, string) error { return b.err }
func (b *brokenCache) Name() string                               { return "broken" }
func (b *brokenCache) Close() error                               { return nil }

// TestGetOrLoad_CacheErrorsFailOpen verifies REQ-CACHE-001: the cache is an
// optimisation, never a dependency. A backend that errors on every call must
// cost latency, not availability -- the request is served from the source and
// the failure is reported to metrics instead of to the caller.
func TestGetOrLoad_CacheErrorsFailOpen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backendErr := errors.New("redis: connection refused")
	cfg := config.Default().Cache
	cfg.Stampede = config.StampedeConfig{}
	cfg.FailOpen = true
	hooks := &recordingHooks{}
	m := NewManager(&brokenCache{err: backendErr}, cfg, hooks.hooks())
	k := key("acme", "resource_status", "R1").String()

	load, calls := countingLoader("from-source")
	res, err := m.GetOrLoad(ctx, k, time.Minute, load)

	testutil.NoError(t, err, "a broken cache must not fail the request")
	testutil.Equal(t, res.Entry.RoutingRule, "from-source", "the source's answer is served")
	testutil.Equal(t, calls.Load(), int64(1), "the loader ran")

	ops := hooks.errorOps()
	testutil.True(t, len(ops) >= 2, "both the failed read and the failed write are reported, got %v", ops)
	testutil.Equal(t, ops[0], "get", "the read failure is attributed to the read")
	testutil.Equal(t, ops[len(ops)-1], "set", "and the write failure to the write")

	t.Run("the other entry points stay quiet too", func(t *testing.T) {
		t.Parallel()
		_, hit := m.Get(ctx, k)
		testutil.False(t, hit, "a broken backend reports a miss")
		_, _, staleOK := m.GetStale(ctx, k)
		testutil.False(t, staleOK, "and has no stale entry to offer")
		m.Invalidate(ctx, k)
		testutil.Error(t, m.InvalidateTenant(ctx, "acme"),
			"an explicit invalidation, unlike a read, does surface its failure")
	})
}

// TestManager_DisabledIsAPassThrough verifies REQ-CACHE-001: with caching off,
// every call site behaves exactly as if the cache were absent. Nothing branches
// on `if cacheEnabled` at the call sites, so the no-op has to be complete.
func TestManager_DisabledIsAPassThrough(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	k := key("acme", "resource_status", "R1").String()

	cases := map[string]*Manager{
		"disabled by configuration": func() *Manager {
			cfg := config.Default().Cache
			cfg.Enabled = false
			return NewManager(NewMemory(16), cfg, NoopHooks())
		}(),
		"no backend configured": NewManager(nil, config.Default().Cache, NoopHooks()),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutil.False(t, m.Enabled(), "caching is off")

			load, calls := countingLoader("from-source")
			res, err := m.GetOrLoad(ctx, k, time.Minute, load)
			testutil.NoError(t, err, "lookup")
			testutil.Equal(t, res.Entry.RoutingRule, "from-source", "the loader answers")
			testutil.Equal(t, res.Layer, "NONE", "no tier is claimed")

			_, err = m.GetOrLoad(ctx, k, time.Minute, load)
			testutil.NoError(t, err, "second lookup")
			testutil.Equal(t, calls.Load(), int64(2), "nothing was remembered between the two")

			_, hit := m.Get(ctx, k)
			testutil.False(t, hit, "Get never hits")
			_, _, staleOK := m.GetStale(ctx, k)
			testutil.False(t, staleOK, "GetStale never hits")
			m.SetNegative(ctx, k)
			m.Invalidate(ctx, k)
			testutil.NoError(t, m.InvalidateTenant(ctx, "acme"), "invalidation is a silent no-op")
		})
	}

	t.Run("a nil manager is safe", func(t *testing.T) {
		t.Parallel()
		var m *Manager
		testutil.False(t, m.Enabled(), "a service wired without a cache must not panic on the read path")
	})
}
