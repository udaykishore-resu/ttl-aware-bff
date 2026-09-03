package resilience

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
)

// RateLimiter admits requests per tenant (or globally) using a token bucket.
//
// Two production concerns shape it:
//
//   - The per-tenant limiter map is bounded and evicted by idleness. A tenant
//     id arrives from a JWT claim; without a bound, a compromised issuer or a
//     buggy client could grow the map without limit.
//   - Limiters are created under a write lock but read under a read lock, so
//     the steady state is read-mostly.
type RateLimiter struct {
	cfg config.RateLimitConfig
	now func() time.Time

	mu       sync.RWMutex
	limiters map[string]*entry
	lastGC   time.Time
}

type entry struct {
	lim *rate.Limiter
	// seen is touched on the read-mostly fast path, where only the read lock is
	// held, and swept by gcLocked under the write lock. It is therefore an
	// atomic rather than a plain time.Time: a plain field here is a real data
	// race between two concurrent Allow calls for the same tenant.
	seen atomic.Int64 // Unix nanoseconds
}

func (e *entry) touch(t time.Time) { e.seen.Store(t.UnixNano()) }

func (e *entry) idleFor(now time.Time) time.Duration {
	return time.Duration(now.UnixNano() - e.seen.Load())
}

// idleEviction is how long an unused tenant limiter is kept. Keeping it for a
// while matters: evicting immediately would reset a tenant's bucket between
// bursts and effectively double their allowance.
const idleEviction = 10 * time.Minute

// NewRateLimiter builds a limiter from configuration.
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		cfg:      cfg,
		now:      time.Now,
		limiters: make(map[string]*entry),
		lastGC:   time.Now(),
	}
}

// Allow reports whether a request from the given tenant may proceed.
func (r *RateLimiter) Allow(tenantID string) bool {
	if !r.cfg.Enabled {
		return true
	}
	key := "global"
	if r.cfg.PerTenant && tenantID != "" {
		key = tenantID
	}

	now := r.now()

	r.mu.RLock()
	e, ok := r.limiters[key]
	r.mu.RUnlock()
	if ok {
		e.touch(now)
		return e.lim.AllowN(now, 1)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check: another goroutine may have created it.
	if e, ok := r.limiters[key]; ok {
		e.touch(now)
		return e.lim.AllowN(now, 1)
	}
	r.gcLocked(now)
	if r.cfg.MaxTenants > 0 && len(r.limiters) >= r.cfg.MaxTenants {
		// The map is full of active tenants. Rather than evicting an active
		// tenant or growing without bound, admit the request: the global
		// bulkheads and the server's own concurrency limits are the backstop.
		return true
	}
	burst := r.cfg.Burst
	if burst < 1 {
		burst = r.cfg.RPS
	}
	e = &entry{lim: rate.NewLimiter(rate.Limit(r.cfg.RPS), burst)}
	e.touch(now)
	r.limiters[key] = e
	return e.lim.AllowN(now, 1)
}

// Size reports how many limiters are tracked, for tests and metrics.
func (r *RateLimiter) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.limiters)
}

// gcLocked evicts idle limiters. Called only on the create path, which bounds
// its cost to the rate at which new tenants appear.
func (r *RateLimiter) gcLocked(now time.Time) {
	if now.Sub(r.lastGC) < time.Minute {
		return
	}
	r.lastGC = now
	for k, e := range r.limiters {
		if e.idleFor(now) > idleEviction {
			delete(r.limiters, k)
		}
	}
}
