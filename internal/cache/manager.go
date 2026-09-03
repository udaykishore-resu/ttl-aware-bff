package cache

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
)

// Layered composes an in-process L1 in front of a distributed L2.
//
// Writes go to both. Reads try L1, then L2, and a L2 hit is promoted into L1
// with the *remaining* lifetime of the L2 entry rather than a fresh L1 TTL,
// so promotion cannot extend an entry's life.
type Layered struct {
	l1    Cache
	l2    Cache
	l1TTL time.Duration
	now   func() time.Time
}

// NewLayered builds a two-tier cache.
func NewLayered(l1, l2 Cache, l1TTL time.Duration) *Layered {
	return &Layered{l1: l1, l2: l2, l1TTL: l1TTL, now: time.Now}
}

// Name implements Cache.
func (l *Layered) Name() string { return "layered" }

// Get implements Cache, reporting which tier answered through LayerOf.
func (l *Layered) Get(ctx context.Context, key string) (*Entry, error) {
	if e, err := l.l1.Get(ctx, key); err == nil {
		return e, nil
	} else if !errors.Is(err, ErrMiss) {
		return nil, err
	}
	e, err := l.l2.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	// Promote with whatever life the entry has left, capped by the L1 TTL.
	remaining := e.CacheTTL - e.Age(l.now())
	if remaining > l.l1TTL {
		remaining = l.l1TTL
	}
	if remaining > 0 {
		_ = l.l1.Set(ctx, key, e, remaining)
	}
	return e, nil
}

// Set implements Cache.
func (l *Layered) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	l1TTL := ttl
	if l1TTL > l.l1TTL {
		l1TTL = l.l1TTL
	}
	_ = l.l1.Set(ctx, key, e, l1TTL)
	return l.l2.Set(ctx, key, e, ttl)
}

// Delete implements Cache.
func (l *Layered) Delete(ctx context.Context, key string) error {
	_ = l.l1.Delete(ctx, key)
	return l.l2.Delete(ctx, key)
}

// DeletePrefix implements Cache.
func (l *Layered) DeletePrefix(ctx context.Context, prefix string) error {
	_ = l.l1.DeletePrefix(ctx, prefix)
	return l.l2.DeletePrefix(ctx, prefix)
}

// Close implements Cache.
func (l *Layered) Close() error {
	_ = l.l1.Close()
	return l.l2.Close()
}

// InL1 reports whether a key is resident in the fast tier. Used only to label
// the cache layer in the response envelope.
func (l *Layered) InL1(ctx context.Context, key string) bool {
	_, err := l.l1.Get(ctx, key)
	return err == nil
}

// ---------------------------------------------------------------------------
// Manager: the cache-aside implementation the application layer uses
// ---------------------------------------------------------------------------

// Loader produces a fresh value on a cache miss.
type Loader func(ctx context.Context) (*Entry, error)

// Result describes how a lookup was satisfied.
type Result struct {
	Entry *Entry
	Hit   bool
	Layer string
	// Stampede reports that this call waited on another goroutine's load
	// rather than performing its own.
	Stampede bool
}

// Manager implements cache-aside with stampede protection, negative caching
// and fail-open error handling.
//
// Stampede protection has two layers, and both are needed:
//
//   - singleflight collapses concurrent identical misses inside one process.
//     This is what stops one popular resource from generating N source calls
//     when N goroutines miss at once (REQ-EDGE-012).
//   - an optional Redis lock collapses them across processes. Losers of the
//     lock do not queue: they perform their own load. Queuing would convert a
//     cache miss into a latency spike for every instance, which is worse than
//     a few duplicate source reads.
type Manager struct {
	cache Cache
	cfg   config.CacheConfig
	sf    singleflight.Group
	now   func() time.Time

	locker Locker
	hooks  Hooks
}

// Locker is the optional cross-process lock used for stampede protection.
type Locker interface {
	AcquireLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, key string)
}

// Hooks reports cache events for metrics without importing observability.
type Hooks struct {
	OnHit   func(layer string)
	OnMiss  func()
	OnError func(op string, err error)
}

// NoopHooks returns hooks that do nothing.
func NoopHooks() Hooks {
	return Hooks{OnHit: func(string) {}, OnMiss: func() {}, OnError: func(string, error) {}}
}

// NewManager builds a cache manager. A nil cache disables caching entirely,
// so call sites never need to branch on whether caching is on.
func NewManager(c Cache, cfg config.CacheConfig, hooks Hooks) *Manager {
	m := &Manager{cache: c, cfg: cfg, now: time.Now, hooks: hooks}
	if l, ok := c.(Locker); ok && cfg.Stampede.Enabled {
		m.locker = l
	}
	if lay, ok := c.(*Layered); ok && cfg.Stampede.Enabled {
		if l2, ok := lay.l2.(Locker); ok {
			m.locker = l2
		}
	}
	return m
}

// WithClock injects the clock the manager uses to stamp and age entries.
//
// It matters that this is the SAME clock the application layer uses. An entry
// stamped by one clock and aged by another produces nonsense: with the service
// on an injected test clock and the cache on the wall clock, every entry looks
// months old and no cache hit is ever possible.
func (m *Manager) WithClock(now func() time.Time) *Manager {
	m.now = now
	return m
}

// Enabled reports whether caching is active.
func (m *Manager) Enabled() bool { return m != nil && m.cfg.Enabled && m.cache != nil }

// Get looks up a key without loading.
//
// A backend error is reported through the returned error, but whether that
// error reaches the caller's response is governed by cache.fail_open: with it
// set (the production default) GetOrLoad swallows it and reads the source
// instead, so a Redis outage costs latency rather than availability.
func (m *Manager) Get(ctx context.Context, key string) (Result, bool) {
	res, ok, _ := m.get(ctx, key, true)
	return res, ok
}

// get is the counted lookup. countMiss exists because GetOrLoad checks the
// cache twice for one logical lookup -- once before entering singleflight and
// once inside it, to catch a value another goroutine has just written. Counting
// both would double the miss total for every cold request and understate the
// hit ratio the SLO is computed from.
func (m *Manager) get(ctx context.Context, key string, countMiss bool) (Result, bool, error) {
	if !m.Enabled() {
		return Result{Layer: string(layerNone)}, false, nil
	}
	miss := func() (Result, bool, error) {
		if countMiss {
			m.hooks.OnMiss()
		}
		return Result{Layer: string(layerNone)}, false, nil
	}

	e, err := m.cache.Get(ctx, key)
	switch {
	case err == nil:
		if e.Expired(m.now()) {
			return miss()
		}
		layer := m.layerFor(ctx, key)
		m.hooks.OnHit(layer)
		return Result{Entry: e, Hit: true, Layer: layer}, true, nil
	case errors.Is(err, ErrMiss):
		return miss()
	default:
		m.hooks.OnError("get", err)
		if countMiss {
			m.hooks.OnMiss()
		}
		return Result{Layer: string(layerNone)}, false, err
	}
}

// GetOrLoad implements cache-aside for one key.
//
// ttl of zero disables caching for this call: the loader runs and its result
// is returned without being stored. That is how request types configured with
// cache_ttl: 0s (execution history, for instance) opt out per-request without
// a special code path.
func (m *Manager) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load Loader) (Result, error) {
	if !m.Enabled() || ttl <= 0 {
		e, err := load(ctx)
		return Result{Entry: e, Layer: string(layerNone)}, err
	}

	res, ok, cacheErr := m.get(ctx, key, true)
	if cacheErr != nil && !m.cfg.FailOpen {
		return Result{Layer: string(layerNone)}, cacheErr
	}
	if ok {
		if !m.shouldEarlyRefresh(res.Entry) {
			return res, nil
		}
		// Serve the cached value now and refresh in the background, so the
		// expiry cliff does not land on a user's request.
		m.backgroundRefresh(ctx, key, ttl, load)
		return res, nil
	}

	// Collapse concurrent misses within this process.
	v, err, shared := m.sf.Do(key, func() (any, error) {
		// Re-check: another goroutine may have populated the key while this
		// one was queuing on singleflight. This lookup is deliberately not
		// counted -- it is the second half of one logical lookup.
		if res, ok, _ := m.get(ctx, key, false); ok {
			return res.Entry, nil
		}

		if m.locker != nil {
			locked, lerr := m.locker.AcquireLock(ctx, key, m.cfg.Stampede.LockTTL.D())
			if lerr != nil {
				m.hooks.OnError("lock", lerr)
			}
			if locked {
				defer m.locker.ReleaseLock(ctx, key)
			}
			// Losing the lock is not a reason to wait: proceed and load.
		}

		e, lerr := load(ctx)
		if lerr != nil {
			return nil, lerr
		}
		m.store(ctx, key, e, ttl)
		return e, nil
	})
	if err != nil {
		return Result{Layer: string(layerNone)}, err
	}
	entry, _ := v.(*Entry)
	return Result{Entry: entry, Layer: string(layerNone), Stampede: shared}, nil
}

// Store writes an entry without a preceding lookup. It exists for the
// strong-consistency path, which must not READ the cache but should still
// populate it for callers at weaker levels.
func (m *Manager) Store(ctx context.Context, key string, e *Entry, ttl time.Duration) {
	if !m.Enabled() || ttl <= 0 || e == nil {
		return
	}
	m.store(ctx, key, e, ttl)
}

// SetNegative caches a "not found" for the configured negative TTL. Negative
// caching matters because a UI polling a resource that does not exist would
// otherwise hammer both sources (REQ-CACHE-007).
func (m *Manager) SetNegative(ctx context.Context, key string) {
	if !m.Enabled() || m.cfg.NegativeTTL <= 0 {
		return
	}
	e := &Entry{
		StoredAt:      m.now(),
		CacheTTL:      m.cfg.NegativeTTL.D(),
		Negative:      true,
		SchemaVersion: EntrySchemaVersion,
	}
	m.store(ctx, key, e, m.cfg.NegativeTTL.D())
}

// Invalidate removes specific keys.
func (m *Manager) Invalidate(ctx context.Context, keys ...string) {
	if !m.Enabled() {
		return
	}
	for _, k := range keys {
		if err := m.cache.Delete(ctx, k); err != nil {
			m.hooks.OnError("delete", err)
		}
	}
}

// InvalidateTenant removes every entry for a tenant. Used by the admin API and
// on tenant offboarding.
func (m *Manager) InvalidateTenant(ctx context.Context, tenantID string) error {
	if !m.Enabled() {
		return nil
	}
	return m.cache.DeletePrefix(ctx, TenantPrefix(m.cfg.KeyPrefix, tenantID))
}

// KeyPrefix exposes the configured prefix so callers can build keys.
func (m *Manager) KeyPrefix() string { return m.cfg.KeyPrefix }

func (m *Manager) store(ctx context.Context, key string, e *Entry, ttl time.Duration) {
	if e == nil {
		return
	}
	// The manager is the authority on when an entry was written, so it stamps
	// StoredAt unconditionally rather than trusting whatever the caller set.
	e.StoredAt = m.now()
	// A degraded OR partial answer is cached briefly so the degradation cannot
	// outlive the incident that produced it. Partial matters as much as
	// degraded: an answer missing its execution half would otherwise be
	// replayed to every caller for the full cache TTL, including long after the
	// execution source came back.
	if (e.Degraded || e.Partial) && m.cfg.NegativeTTL > 0 && ttl > m.cfg.NegativeTTL.D() {
		ttl = m.cfg.NegativeTTL.D()
	}
	// The logical lifetime is what the response envelope reports and what Get
	// enforces. The physical lifetime is longer, so that an entry survives its
	// own expiry and remains reachable by the stale-serve path when every
	// source is down. Nothing on the normal read path can see it.
	e.CacheTTL = ttl
	e.SchemaVersion = EntrySchemaVersion
	physical := ttl + m.cfg.StaleGrace.D()
	if err := m.cache.Set(ctx, key, e, physical); err != nil {
		m.hooks.OnError("set", err)
	}
}

// GetStale returns an entry even when it has outlived its cache TTL, up to the
// configured stale grace. It is the last resort of the degradation ladder:
// every source is unavailable, the routing decision was NONE, and an old
// answer clearly labelled as stale beats a 503 (REQ-RES-007, REQ-EDGE-005).
//
// It is a separate method rather than a flag on Get so that no ordinary read
// path can reach expired data by accident.
func (m *Manager) GetStale(ctx context.Context, key string) (*Entry, string, bool) {
	if !m.Enabled() {
		return nil, string(layerNone), false
	}
	e, err := m.cache.Get(ctx, key)
	if err != nil {
		if !errors.Is(err, ErrMiss) {
			m.hooks.OnError("get_stale", err)
		}
		return nil, string(layerNone), false
	}
	if e.Negative {
		return nil, string(layerNone), false
	}
	return e, m.layerFor(ctx, key), true
}

func (m *Manager) shouldEarlyRefresh(e *Entry) bool {
	r := m.cfg.Stampede.EarlyRefreshRatio
	if !m.cfg.Stampede.Enabled || r <= 0 || r >= 1 || e == nil || e.CacheTTL <= 0 || e.Negative {
		return false
	}
	return e.Age(m.now()) >= time.Duration(float64(e.CacheTTL)*r)
}

// backgroundRefresh reloads a key without blocking the caller. It uses
// singleflight so only one refresh runs per key, and a detached context with a
// bounded lifetime so the refresh is not cancelled when the triggering request
// finishes.
func (m *Manager) backgroundRefresh(ctx context.Context, key string, ttl time.Duration, load Loader) {
	// WithoutCancel keeps the request's VALUES -- tenant, correlation id,
	// principal -- while dropping its cancellation, so the refresh outlives the
	// request that triggered it but still reaches the sources attributed and
	// traceable. Wrapping context.Background() instead would drop the values
	// too, and the sources would see an anonymous, tenant-less call.
	refreshCtx := context.WithoutCancel(ctx)
	startedAt := m.now()

	go func() {
		// Bounded by a source-call budget, not by the cache TTL: the two are
		// unrelated dimensions, and using the TTL means a short-lived entry can
		// never finish refreshing while a long-lived one holds a goroutine for
		// far longer than any source is allowed to take.
		callCtx, cancel := context.WithTimeout(refreshCtx, refreshBudget)
		defer cancel()

		_, err, _ := m.sf.Do("refresh:"+key, func() (any, error) {
			e, err := load(callCtx)
			if err != nil {
				return nil, err
			}
			// A foreground miss may have completed while this refresh was in
			// flight. Overwriting it would replace newer data with older and
			// re-stamp it as freshly stored.
			if cur, ok, _ := m.get(callCtx, key, false); ok && cur.Entry.StoredAt.After(startedAt) {
				return cur.Entry, nil
			}
			m.store(callCtx, key, e, ttl)
			return e, nil
		})
		if err != nil {
			m.hooks.OnError("refresh", err)
		}
	}()
}

// refreshBudget bounds a background refresh. It is generous relative to any
// single source call and short relative to any cache lifetime.
const refreshBudget = 10 * time.Second

type layerName string

const (
	layerNone layerName = "NONE"
	layerL1   layerName = "L1"
	layerL2   layerName = "L2"
)

func (m *Manager) layerFor(ctx context.Context, key string) string {
	lay, ok := m.cache.(*Layered)
	if !ok {
		switch m.cache.Name() {
		case "memory":
			return string(layerL1)
		case "redis":
			return string(layerL2)
		}
		return string(layerNone)
	}
	if lay.InL1(ctx, key) {
		return string(layerL1)
	}
	return string(layerL2)
}

// Build constructs the configured cache backend.
func Build(cfg config.CacheConfig) (Cache, error) {
	if !cfg.Enabled || cfg.Backend == "none" {
		return nil, nil
	}
	switch cfg.Backend {
	case "memory":
		return NewMemory(cfg.L1.MaxEntries), nil
	case "redis":
		return NewRedis(cfg.Redis)
	case "layered":
		r, err := NewRedis(cfg.Redis)
		if err != nil {
			return nil, err
		}
		return NewLayered(NewMemory(cfg.L1.MaxEntries), r, cfg.L1.TTL.D()), nil
	default:
		return nil, errors.New("cache: unknown backend " + cfg.Backend)
	}
}
