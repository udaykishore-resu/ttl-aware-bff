// Package freshness owns the one question the whole BFF is built around: is
// the operational source's copy of this record recent enough to answer with?
//
// It is deliberately separate from the router. The router decides what to do
// with an answer; this package only produces the answer, and it produces it
// the same way whether the input came from a cheap pre-fetch probe or from a
// record the BFF has just read.
//
// Traceability: REQ-TTL-001..REQ-TTL-009, REQ-EDGE-009, REQ-EDGE-010.
package freshness

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
)

// Evaluation is the outcome of applying a TTL to an observation.
type Evaluation struct {
	domain.Freshness
	// Probed records that the observation came from a pre-fetch probe rather
	// than from data the BFF already had.
	Probed bool
	// FromCache records that the observation came from the probe cache.
	FromCache bool
	// SkewSeconds is the measured disagreement between the source's clock and
	// the BFF's, positive when the source is ahead.
	SkewSeconds float64
	// NotFound records that the source has no such record.
	NotFound bool
}

// Evaluate applies a TTL to an observation and returns the canonical freshness.
//
// The age calculation is the subtle part, and it works like this:
//
//   - If the source reported its own current time, age is
//     sourceTime - lastUpdated. Both values come from the same clock, so the
//     result is immune to any disagreement between that clock and the BFF's.
//     This is the normal path.
//   - If the source did not report its time, age falls back to
//     bffNow - lastUpdated, and any disagreement between the clocks lands
//     directly in the age. A tolerance absorbs small skew.
//   - A record timestamped in the future is not treated as infinitely fresh.
//     Within tolerance the age is clamped to zero; beyond tolerance the
//     evaluation is UNKNOWN, because a source claiming to have refreshed a
//     record next Tuesday is telling us its clock is wrong, and trusting it
//     would let a badly-skewed source pin every record as permanently fresh
//     (REQ-EDGE-010).
//
// A TTL of zero means "never satisfied by age": the request type demands a
// live read. It is not the same as an absent TTL.
func Evaluate(obs datasource.Observation, ttl, skewTolerance time.Duration, now time.Time) Evaluation {
	ev := Evaluation{
		Freshness: domain.Freshness{
			State:       domain.FreshnessUnknown,
			TTL:         ttl,
			ObservedAt:  obs.LastUpdated,
			EvaluatedAt: now,
			Source:      domain.SourceOperational,
			Version:     obs.Version,
		},
		NotFound: !obs.Found,
	}

	if !obs.Found || obs.LastUpdated.IsZero() {
		return ev
	}

	reference := now
	if !obs.SourceTime.IsZero() {
		reference = obs.SourceTime
		ev.SkewSeconds = obs.SourceTime.Sub(now).Seconds()
		if abs(obs.SourceTime.Sub(now)) > skewTolerance {
			ev.SkewCorrected = true
		}
	}

	age := reference.Sub(obs.LastUpdated)
	switch {
	case age >= 0:
		// Normal case.
	case -age <= skewTolerance:
		// Slightly-in-the-future timestamp: treat as just-refreshed.
		age = 0
		ev.SkewCorrected = true
	default:
		// Implausibly future timestamp: refuse to judge.
		ev.SkewCorrected = true
		ev.Age = 0
		ev.State = domain.FreshnessUnknown
		return ev
	}

	ev.Age = age
	if ttl <= 0 {
		// TTL 0 means the request type will not accept age-based satisfaction.
		ev.State = domain.FreshnessStale
		return ev
	}
	if age <= ttl {
		ev.State = domain.FreshnessFresh
	} else {
		ev.State = domain.FreshnessStale
	}
	return ev
}

// EvaluateFreshness applies a TTL to a freshness value the BFF already holds
// (from a record it has just read, or from a cache entry). It is the same
// calculation as Evaluate without a source clock to lean on.
func EvaluateFreshness(f domain.Freshness, ttl, skewTolerance time.Duration, now time.Time) Evaluation {
	return Evaluate(datasource.Observation{
		Found:       !f.ObservedAt.IsZero(),
		LastUpdated: f.ObservedAt,
		Version:     f.Version,
	}, ttl, skewTolerance, now)
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager performs freshness probes, memoising them briefly.
//
// The probe cache is not the response cache and its TTL is not a freshness
// TTL. It bounds how often the BFF asks the source "how old is this?", which
// is a rate-limiting concern; it does not change how old the data may be. The
// cached observation carries the source's own timestamps, so a probe result
// reused 500ms later yields an age 500ms larger, not a stale "fresh" verdict.
type Manager struct {
	probe datasource.FreshnessProbe

	mu    sync.Mutex
	cache map[string]cachedProbe
	ttl   time.Duration
	now   func() time.Time

	// maxEntries bounds the memo so a scan across many resource ids cannot
	// grow it without limit.
	maxEntries int

	hooks Hooks
}

type cachedProbe struct {
	obs      datasource.Observation
	storedAt time.Time
}

// age returns the memoised observation advanced by the time it has spent in
// the memo.
//
// This is the whole reason the memo is safe. The observation's LastUpdated is a
// fact about the source and does not change, but its SourceTime is the instant
// the source answered -- so reusing the pair verbatim would report the age the
// record had when the probe was made, not the age it has now. Advancing
// SourceTime by the elapsed time keeps the calculation inside the source's
// clock domain (which is what makes it skew-proof) while still accounting for
// the wall time that has passed, so a memoised probe can never make a record
// look fresher than it is (REQ-TTL-007).
func (c cachedProbe) age(elapsed time.Duration) datasource.Observation {
	out := c.obs
	if !out.SourceTime.IsZero() {
		out.SourceTime = out.SourceTime.Add(elapsed)
	}
	return out
}

// Hooks reports freshness events for metrics.
type Hooks struct {
	OnProbe     func(ok bool, d time.Duration)
	OnSkew      func(seconds float64)
	OnEvaluated func(state domain.FreshnessState, age time.Duration)
}

// NoopHooks returns hooks that do nothing.
func NoopHooks() Hooks {
	return Hooks{
		OnProbe:     func(bool, time.Duration) {},
		OnSkew:      func(float64) {},
		OnEvaluated: func(domain.FreshnessState, time.Duration) {},
	}
}

// NewManager builds a freshness manager. probeCacheTTL of zero disables the
// memo entirely.
func NewManager(probe datasource.FreshnessProbe, probeCacheTTL time.Duration, hooks Hooks) *Manager {
	return &Manager{
		probe:      probe,
		cache:      make(map[string]cachedProbe),
		ttl:        probeCacheTTL,
		now:        time.Now,
		maxEntries: 50000,
		hooks:      hooks,
	}
}

// WithClock injects a clock for deterministic tests.
func (m *Manager) WithClock(now func() time.Time) *Manager {
	m.now = now
	return m
}

// Probe returns the source's freshness observation for a resource.
//
// A probe failure is NOT an error to the caller: it returns an observation
// with Found=false and the error separately, so the router can apply its
// on_unknown_freshness policy instead of failing the request. A BFF that
// cannot ask "how old is this?" should still be able to answer the request.
func (m *Manager) Probe(ctx context.Context, tenantID, resourceID string) (datasource.Observation, bool, error) {
	if m == nil || m.probe == nil {
		return datasource.Observation{}, false, nil
	}
	key := tenantID + "\x00" + resourceID

	if m.ttl > 0 {
		m.mu.Lock()
		c, ok := m.cache[key]
		m.mu.Unlock()
		if ok {
			if elapsed := m.now().Sub(c.storedAt); elapsed < m.ttl {
				return c.age(elapsed), true, nil
			}
		}
	}

	start := m.now()
	obs, err := m.probe.ProbeFreshness(ctx, tenantID, resourceID)
	m.hooks.OnProbe(err == nil, m.now().Sub(start))
	if err != nil {
		return datasource.Observation{}, false, err
	}

	if m.ttl > 0 {
		m.mu.Lock()
		if len(m.cache) >= m.maxEntries {
			// Cheap bound: drop everything rather than maintain an LRU for a
			// memo whose entries live for a second or two anyway.
			m.cache = make(map[string]cachedProbe, m.maxEntries/2)
		}
		m.cache[key] = cachedProbe{obs: obs, storedAt: m.now()}
		m.mu.Unlock()
	}
	return obs, false, nil
}

// Assess probes and evaluates in one step, which is what the router wants.
func (m *Manager) Assess(ctx context.Context, tenantID, resourceID string, ttl, skewTolerance time.Duration) (Evaluation, error) {
	obs, fromCache, err := m.Probe(ctx, tenantID, resourceID)
	if err != nil {
		return Evaluation{Freshness: domain.Freshness{
			State: domain.FreshnessUnknown, TTL: ttl, EvaluatedAt: m.now(), Source: domain.SourceOperational,
		}}, err
	}
	ev := Evaluate(obs, ttl, skewTolerance, m.now())
	ev.Probed = true
	ev.FromCache = fromCache
	if ev.SkewCorrected {
		m.hooks.OnSkew(ev.SkewSeconds)
	}
	m.hooks.OnEvaluated(ev.State, ev.Age)
	return ev, nil
}

// Invalidate drops the memoised probe for a resource. Called after any write
// path the BFF becomes aware of.
func (m *Manager) Invalidate(tenantID, resourceID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.cache, tenantID+"\x00"+resourceID)
	m.mu.Unlock()
}

// Size reports the memo size, for tests and metrics.
func (m *Manager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cache)
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
