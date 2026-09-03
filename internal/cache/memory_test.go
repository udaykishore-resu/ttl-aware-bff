package cache

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
)

// memFixture builds an in-process cache on a manually advanced clock, so no
// expiry test has to sleep.
func memFixture(maxItems int) (*Memory, *testutil.Clock) {
	clk := testutil.NewClock(base)
	return NewMemory(maxItems).WithClock(clk.Now), clk
}

// entry builds a distinguishable entry.
func entry(rule string) *Entry {
	return &Entry{
		Payload:     []byte(rule),
		RoutingRule: rule,
		StoredAt:    base,
		CacheTTL:    time.Minute,
		Sources:     []domain.SourceKind{domain.SourceOperational},
	}
}

// TestMemory_GetSetDelete verifies REQ-CACHE-001 at the storage layer: a miss
// is reported as ErrMiss rather than as a nil entry, so a caller cannot confuse
// "absent" with "present but empty".
func TestMemory_GetSetDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _ := memFixture(16)

	_, err := m.Get(ctx, "absent")
	testutil.True(t, errors.Is(err, ErrMiss), "an unknown key is a miss, got %v", err)

	testutil.NoError(t, m.Set(ctx, "k1", entry("ttl.fresh"), time.Minute), "set")
	got, err := m.Get(ctx, "k1")
	testutil.NoError(t, err, "get after set")
	testutil.Equal(t, got.RoutingRule, "ttl.fresh", "the stored entry comes back")
	testutil.Equal(t, m.Len(), 1, "one entry is held")

	t.Run("set replaces in place", func(t *testing.T) {
		testutil.NoError(t, m.Set(ctx, "k1", entry("ttl.stale"), time.Minute), "overwrite")
		got, err := m.Get(ctx, "k1")
		testutil.NoError(t, err, "get after overwrite")
		testutil.Equal(t, got.RoutingRule, "ttl.stale", "the newer entry wins")
		testutil.Equal(t, m.Len(), 1, "and does not add a second entry")
	})

	t.Run("delete removes the key", func(t *testing.T) {
		testutil.NoError(t, m.Delete(ctx, "k1"), "delete")
		_, err := m.Get(ctx, "k1")
		testutil.True(t, errors.Is(err, ErrMiss), "a deleted key is a miss")
		testutil.Equal(t, m.Len(), 0, "and is no longer held")
	})

	t.Run("deleting an absent key is not an error", func(t *testing.T) {
		testutil.NoError(t, m.Delete(ctx, "never-existed"), "delete of an absent key")
	})

	t.Run("a non-positive TTL stores nothing", func(t *testing.T) {
		testutil.NoError(t, m.Set(ctx, "k0", entry("x"), 0), "set with a zero TTL")
		_, err := m.Get(ctx, "k0")
		testutil.True(t, errors.Is(err, ErrMiss),
			"an entry that would be born expired is never written at all")
	})
}

// TestMemory_DeletePrefix verifies REQ-MT-005: tenant-scoped invalidation is a
// prefix delete, and it must reach every key of that tenant and no key of
// another.
func TestMemory_DeletePrefix(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _ := memFixture(16)

	acme := []string{
		key("acme", "resource_status", "R1").String(),
		key("acme", "resource_read", "R1").String(),
		key("acme", "resource_status", "R2").String(),
	}
	globex := key("globex", "resource_status", "R1").String()

	for _, k := range append(append([]string{}, acme...), globex) {
		testutil.NoError(t, m.Set(ctx, k, entry("x"), time.Minute), "seed %s", k)
	}
	testutil.Equal(t, m.Len(), 4, "seeded")

	testutil.NoError(t, m.DeletePrefix(ctx, TenantPrefix(prefix, "acme")), "delete prefix")

	for _, k := range acme {
		_, err := m.Get(ctx, k)
		testutil.True(t, errors.Is(err, ErrMiss), "%s should have been evicted", k)
	}
	_, err := m.Get(ctx, globex)
	testutil.NoError(t, err, "another tenant's entry must survive")
	testutil.Equal(t, m.Len(), 1, "exactly one entry remains")
}

// TestMemory_TTLExpiry verifies REQ-CACHE-001: an entry is unreachable once its
// physical lifetime has run out, and the eviction is lazy -- it happens on the
// read that discovers it, so no sweeper goroutine is needed per instance.
func TestMemory_TTLExpiry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, clk := memFixture(16)
	testutil.NoError(t, m.Set(ctx, "k", entry("x"), 30*time.Second), "set")

	clk.Advance(29 * time.Second)
	_, err := m.Get(ctx, "k")
	testutil.NoError(t, err, "still inside the TTL")

	clk.Advance(time.Second)
	_, err = m.Get(ctx, "k")
	testutil.NoError(t, err, "exactly at the expiry instant the entry is still live")

	clk.Advance(time.Nanosecond)
	_, err = m.Get(ctx, "k")
	testutil.True(t, errors.Is(err, ErrMiss), "one nanosecond past expiry it is gone, got %v", err)
	testutil.Equal(t, m.Len(), 0, "and the read that found it expired dropped it")
}

// TestMemory_LRUEvictsLeastRecentlyUsed verifies REQ-CACHE-008: the L1 tier is
// bounded, and the entry it gives up is the least recently *used*, not the
// least recently inserted. Evicting by insertion order would throw away exactly
// the entry a user sitting on one screen keeps re-reading.
func TestMemory_LRUEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _ := memFixture(2)

	testutil.NoError(t, m.Set(ctx, "a", entry("a"), time.Minute), "set a")
	testutil.NoError(t, m.Set(ctx, "b", entry("b"), time.Minute), "set b")

	// Reading "a" makes it the most recently used, even though it was inserted
	// first.
	_, err := m.Get(ctx, "a")
	testutil.NoError(t, err, "read a")

	testutil.NoError(t, m.Set(ctx, "c", entry("c"), time.Minute), "set c, exceeding the bound")

	testutil.Equal(t, m.Len(), 2, "the bound is enforced")
	_, err = m.Get(ctx, "a")
	testutil.NoError(t, err, "the recently used entry survives")
	_, err = m.Get(ctx, "b")
	testutil.True(t, errors.Is(err, ErrMiss), "the least recently used entry was evicted")
	_, err = m.Get(ctx, "c")
	testutil.NoError(t, err, "and the new entry is resident")

	t.Run("overwriting also refreshes recency", func(t *testing.T) {
		// a and c are resident; touch a by writing it, then insert d.
		testutil.NoError(t, m.Set(ctx, "a", entry("a2"), time.Minute), "overwrite a")
		testutil.NoError(t, m.Set(ctx, "d", entry("d"), time.Minute), "insert d")
		_, err := m.Get(ctx, "c")
		testutil.True(t, errors.Is(err, ErrMiss), "c was the least recently used and lost its place")
		got, err := m.Get(ctx, "a")
		testutil.NoError(t, err, "a survived because writing it counts as using it")
		testutil.Equal(t, got.RoutingRule, "a2", "and it holds the newer value")
	})

	t.Run("a bound below one is refused", func(t *testing.T) {
		// A misconfigured max_entries must not produce a cache that evicts
		// everything it stores.
		z := NewMemory(0)
		testutil.NoError(t, z.Set(ctx, "k", entry("k"), time.Minute), "set")
		_, err := z.Get(ctx, "k")
		testutil.NoError(t, err, "a zero bound falls back to a usable default")
	})
}

// TestMemory_GetReturnsACopy verifies that the caller cannot reach into the
// cache through the value it was handed. A response assembled from a cache hit
// is decorated per-request -- correlation id, warnings, cache layer -- and if
// that decoration landed on the stored entry, every subsequent hit would carry
// another request's metadata.
func TestMemory_GetReturnsACopy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _ := memFixture(16)

	original := entry("ttl.fresh")
	testutil.NoError(t, m.Set(ctx, "k", original, time.Minute), "set")

	// Mutating the value handed back must not be visible to the next reader.
	got, err := m.Get(ctx, "k")
	testutil.NoError(t, err, "first read")
	got.RoutingRule = "tampered"
	got.Degraded = true
	got.Partial = true

	again, err := m.Get(ctx, "k")
	testutil.NoError(t, err, "second read")
	testutil.Equal(t, again.RoutingRule, "ttl.fresh", "the stored routing rule is intact")
	testutil.False(t, again.Degraded, "the stored entry was not marked degraded")
	testutil.False(t, again.Partial, "nor partial")

	t.Run("and the writer's value is copied on the way in", func(t *testing.T) {
		// The caller keeps a pointer to what it stored; mutating it afterwards
		// must not rewrite the cache.
		original.RoutingRule = "mutated-after-store"
		original.Degraded = true

		held, err := m.Get(ctx, "k")
		testutil.NoError(t, err, "read after the writer mutated its own copy")
		testutil.Equal(t, held.RoutingRule, "ttl.fresh", "the cache kept its own copy")
		testutil.False(t, held.Degraded, "and is unaffected by the writer's later edits")
	})
}

// TestMemory_ConcurrentAccess verifies REQ-CACHE-008 under the race detector:
// the L1 tier is shared by every in-flight request, so every operation on it
// has to be safe for concurrent use. The assertions are deliberately weak --
// what is being tested is that -race reports nothing.
func TestMemory_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m, _ := memFixture(64)

	const workers = 16
	const iterations = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				k := key("t"+strconv.Itoa(w%4), "resource_status", "R"+strconv.Itoa(i%8)).String()
				switch i % 5 {
				case 0, 1:
					_ = m.Set(ctx, k, entry("r"+strconv.Itoa(i)), time.Minute)
				case 2, 3:
					if e, err := m.Get(ctx, k); err == nil {
						// Read a field, so a torn entry would be observable.
						_ = e.RoutingRule
					}
				case 4:
					if i%20 == 4 {
						_ = m.DeletePrefix(ctx, TenantPrefix(prefix, "t"+strconv.Itoa(w%4)))
					} else {
						_ = m.Delete(ctx, k)
					}
				}
				_ = m.Len()
			}
		}(w)
	}
	wg.Wait()

	testutil.True(t, m.Len() <= 64, "the bound held throughout, got %d entries", m.Len())
	testutil.NoError(t, m.Close(), "close")
}
