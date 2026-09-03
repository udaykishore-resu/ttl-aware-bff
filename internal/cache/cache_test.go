package cache

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
)

var base = time.Date(2026, 6, 8, 14, 0, 0, 0, time.UTC)

const prefix = "bff:v1"

// key is the shorthand every key test uses.
func key(tenant, requestType, resource string) Key {
	return Key{Prefix: prefix, TenantID: tenant, RequestType: requestType, ResourceID: resource}
}

// TestKey_StringCarriesEveryIsolationDimension verifies REQ-CACHE-002: tenant,
// request type and resource are structural segments of the key, not values
// buried in a payload or appended as a suffix. Building keys through this type
// is what makes forgetting the tenant impossible rather than merely unlikely.
func TestKey_StringCarriesEveryIsolationDimension(t *testing.T) {
	t.Parallel()

	k := key("acme", "resource_status", "R1")
	got := k.String()

	testutil.True(t, strings.HasPrefix(got, prefix+":"), "the configured prefix leads the key, got %q", got)
	testutil.True(t, strings.Contains(got, ":t=acme"), "the tenant is a segment, got %q", got)
	testutil.True(t, strings.Contains(got, ":rt=resource_status"), "the request type is a segment, got %q", got)
	testutil.True(t, strings.Contains(got, ":r=R1"), "the resource is a segment, got %q", got)
	testutil.True(t, strings.Index(got, ":t=") < strings.Index(got, ":rt="),
		"the tenant comes first, so a tenant-scoped prefix delete is possible, got %q", got)

	t.Run("the sub-resource and variant segments are optional", func(t *testing.T) {
		t.Parallel()
		testutil.False(t, strings.Contains(got, ":s="), "no sub-resource was set")
		testutil.False(t, strings.Contains(got, ":v="), "and no variant")

		sub := k
		sub.Sub = "E-1"
		testutil.True(t, strings.Contains(sub.String(), ":s=E-1"), "a sub-resource appears when set")
		testutil.NotEqual(t, sub.String(), got, "and it changes the key")
	})

	t.Run("empty segments are still segments", func(t *testing.T) {
		t.Parallel()
		// A missing tenant must not collapse the key into one that a populated
		// tenant could also produce. The placeholder therefore uses a character
		// outside the safe segment set, so no real tenant id can render as it.
		empty := key("", "resource_status", "R1").String()
		testutil.True(t, strings.Contains(empty, ":t="+emptySegment),
			"an empty tenant renders as a placeholder, got %q", empty)
		testutil.NotEqual(t, empty, got, "and does not alias a real tenant's key")
		testutil.NotEqual(t, empty, key("-", "resource_status", "R1").String(),
			"and a tenant literally named \"-\" is a different key again")
	})
}

// TestKey_TenantsNeverShareAKey verifies REQ-MT-005: two tenants asking for the
// same resource, in the same way, must produce different keys. This is the
// highest-severity failure mode a multi-tenant cache has, so it is asserted
// across ordinary ids, ids that differ only by case, and ids crafted to look
// like key syntax.
func TestKey_TenantsNeverShareAKey(t *testing.T) {
	t.Parallel()

	cases := []struct{ a, b string }{
		{"acme", "globex"},
		{"acme", "acme2"},
		{"acme", "ACME"},
		{"acme", "acme-eu"},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			t.Parallel()
			ka := key(tc.a, "resource_status", "R1").String()
			kb := key(tc.b, "resource_status", "R1").String()
			testutil.NotEqual(t, ka, kb, "tenants %q and %q must not share a cache key", tc.a, tc.b)
		})
	}
}

// TestKey_HostileResourceIDsCannotForgeAnotherTenantsKey verifies REQ-MT-005
// and REQ-CACHE-002: a resource id is caller-controlled, so it is never allowed
// into the key verbatim unless it is plainly safe. Anything else collapses to a
// hash, which cannot contain the delimiter and therefore cannot impersonate
// another tenant's segment.
func TestKey_HostileResourceIDsCannotForgeAnotherTenantsKey(t *testing.T) {
	t.Parallel()

	odd := map[string]string{
		"contains the delimiter":  "R1:rt=resource_status",
		"forges a tenant segment": "R1:t=victim",
		"contains a colon":        "a:b",
		"contains a slash":        "a/b",
		"contains unicode":        "réssource-ünicode",
		"contains a newline":      "R1\nR2",
		"contains a space":        "R 1",
		"is long and unsafe":      strings.Repeat("ré/", 512),
	}

	seen := map[string]string{}
	for name, id := range odd {
		t.Run(name, func(t *testing.T) {
			rendered := key("attacker", "resource_status", id).String()
			segment := rendered[strings.LastIndex(rendered, ":r=")+3:]

			testutil.True(t, strings.HasPrefix(segment, "h_"),
				"an unsafe id must be hashed, got segment %q", segment)
			testutil.False(t, strings.Contains(segment, ":"),
				"a hashed segment cannot contain the delimiter, got %q", segment)
			testutil.True(t, len(segment) < 64, "and it is bounded in length, got %d bytes", len(segment))

			// The victim tenant's key for the plain resource id is unreachable
			// however the id is spelled.
			testutil.NotEqual(t, rendered, key("victim", "resource_status", "R1").String(),
				"a crafted resource id must not resolve to another tenant's key")

			// Two different odd ids must not collapse onto one another: dropping
			// the unsafe characters (rather than hashing) would make "a:b" and
			// "ab" the same entry.
			if prev, ok := seen[rendered]; ok {
				t.Fatalf("resource ids %q and %q collided onto key %q", prev, id, rendered)
			}
			seen[rendered] = id
		})
	}

	t.Run("a stripped spelling is not the same key", func(t *testing.T) {
		t.Parallel()
		testutil.NotEqual(t,
			key("acme", "resource_status", "a:b").String(),
			key("acme", "resource_status", "ab").String(),
			"hashing, not stripping, is what keeps two odd ids apart")
	})

	t.Run("a long but syntactically safe id is kept verbatim", func(t *testing.T) {
		t.Parallel()
		// Length alone is not a reason to hash: the id contains no delimiter,
		// so it cannot reach another tenant's segment however long it is. This
		// documents the deliberate boundary of the sanitiser -- it defends the
		// key structure, not the key length.
		long := strings.Repeat("x", 4096)
		rendered := key("acme", "resource_status", long).String()
		testutil.True(t, strings.HasSuffix(rendered, ":r="+long), "a safe id survives intact")
		testutil.True(t, strings.HasPrefix(rendered, TenantPrefix(prefix, "acme")),
			"and stays inside its tenant")
	})
}

// TestKey_Variant verifies REQ-CACHE-002: anything else that changes the
// response body -- pagination, a field projection, the caller's permissions --
// is part of the key. The variant is hashed over sorted pairs, so the same
// inputs always produce the same key regardless of map iteration order.
func TestKey_Variant(t *testing.T) {
	t.Parallel()

	plain := key("acme", "resource_read", "R1")
	withVariant := plain
	withVariant.Variant = map[string]string{"fields": "status", "limit": "25"}

	testutil.NotEqual(t, withVariant.String(), plain.String(), "a variant changes the key")
	testutil.True(t, strings.Contains(withVariant.String(), ":v="), "and appears as its own segment")

	t.Run("order independent", func(t *testing.T) {
		t.Parallel()
		reordered := plain
		reordered.Variant = map[string]string{"limit": "25", "fields": "status"}
		testutil.Equal(t, reordered.String(), withVariant.String(),
			"the same variant written in a different order is the same key")
	})

	t.Run("different variants differ", func(t *testing.T) {
		t.Parallel()
		other := plain
		other.Variant = map[string]string{"fields": "status", "limit": "50"}
		testutil.NotEqual(t, other.String(), withVariant.String(), "a changed value changes the key")

		// The pair separator matters: without it, {"ab":"c"} and {"a":"bc"}
		// would hash identically.
		ambiguousA, ambiguousB := plain, plain
		ambiguousA.Variant = map[string]string{"ab": "c"}
		ambiguousB.Variant = map[string]string{"a": "bc"}
		testutil.NotEqual(t, ambiguousA.String(), ambiguousB.String(),
			"variant pairs must be delimited, or two different projections share an entry")
	})

	t.Run("an empty variant map is no variant", func(t *testing.T) {
		t.Parallel()
		emptyVariant := plain
		emptyVariant.Variant = map[string]string{}
		testutil.Equal(t, emptyVariant.String(), plain.String(), "an empty variant adds no segment")
	})
}

// TestTenantPrefix verifies REQ-MT-005: the tenant prefix covers every key of
// one tenant, which is what makes tenant-scoped invalidation a single prefix
// delete rather than a scan of the whole keyspace.
func TestTenantPrefix(t *testing.T) {
	t.Parallel()

	acme := TenantPrefix(prefix, "acme")

	for _, rt := range []string{"resource_status", "resource_read", "execution_history"} {
		for _, id := range []string{"R1", "R2", "a/b"} {
			k := key("acme", rt, id).String()
			testutil.True(t, strings.HasPrefix(k, acme),
				"tenant prefix %q must cover key %q", acme, k)
		}
	}

	t.Run("it does not cover another tenant", func(t *testing.T) {
		t.Parallel()
		other := key("globex", "resource_status", "R1").String()
		testutil.False(t, strings.HasPrefix(other, acme),
			"tenant prefix %q must not cover another tenant's key %q", acme, other)
		testutil.NotEqual(t, TenantPrefix(prefix, "globex"), acme, "each tenant has its own prefix")
	})

	t.Run("the schema version is part of the prefix", func(t *testing.T) {
		t.Parallel()
		// A rolling deploy that changes the entry shape must not have its old
		// entries served or its invalidation aimed at the wrong generation.
		testutil.True(t, strings.Contains(acme, ":e"+itoa(EntrySchemaVersion)+":"),
			"prefix %q carries the entry schema version", acme)
	})
}

// TestResourceKeysForTypes verifies REQ-CACHE-002: invalidating one resource
// enumerates the exact keys to drop. Exact keys beat a wildcard scan, because
// KEYS or SCAN against a busy Redis is how a cache invalidation becomes an
// outage.
func TestResourceKeysForTypes(t *testing.T) {
	t.Parallel()

	types := []string{"resource_status", "resource_read"}
	got := ResourceKeysForTypes(prefix, "acme", "R1", types)

	testutil.Equal(t, len(got), len(types), "one key per request type")
	for i, rt := range types {
		testutil.Equal(t, got[i], key("acme", rt, "R1").String(),
			"the enumerated key must be byte-identical to the one a read would build")
	}

	t.Run("no request types yields no keys", func(t *testing.T) {
		t.Parallel()
		testutil.Equal(t, len(ResourceKeysForTypes(prefix, "acme", "R1", nil)), 0, "nothing to evict")
	})

	t.Run("keys stay inside the tenant", func(t *testing.T) {
		t.Parallel()
		for _, k := range got {
			testutil.True(t, strings.HasPrefix(k, TenantPrefix(prefix, "acme")),
				"resource invalidation must never reach outside the tenant, got %q", k)
		}
	})
}

// ---------------------------------------------------------------------------
// Entry
// ---------------------------------------------------------------------------

// TestEntry_Age verifies that an entry reports how long it has been held,
// measured from the moment the BFF wrote it.
func TestEntry_Age(t *testing.T) {
	t.Parallel()

	clk := testutil.NewClock(base)
	e := &Entry{StoredAt: clk.Now(), CacheTTL: time.Minute}

	testutil.Equal(t, e.Age(clk.Now()), time.Duration(0), "a just-written entry has no age")
	clk.Advance(90 * time.Second)
	testutil.Equal(t, e.Age(clk.Now()), 90*time.Second, "age tracks wall time")
}

// TestEntry_Expired verifies REQ-CACHE-001: the cache TTL bounds how long a
// computed answer may be reused. The boundary is asserted exactly, and a
// zero TTL means "never reusable" rather than "reusable forever" -- the latter
// is what an unset field would silently mean.
func TestEntry_Expired(t *testing.T) {
	t.Parallel()

	const ttl = 30 * time.Second
	cases := []struct {
		name string
		ttl  time.Duration
		age  time.Duration
		want bool
	}{
		{"just written", ttl, 0, false},
		{"inside the TTL", ttl, 10 * time.Second, false},
		{"one nanosecond inside", ttl, ttl - time.Nanosecond, false},
		{"exactly at the TTL", ttl, ttl, true},
		{"past the TTL", ttl, ttl + time.Second, true},
		{"a zero TTL is never reusable", 0, 0, true},
		{"a negative TTL is never reusable", -time.Second, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := testutil.NewClock(base)
			e := &Entry{StoredAt: clk.Now(), CacheTTL: tc.ttl}
			clk.Advance(tc.age)
			testutil.Equal(t, e.Expired(clk.Now()), tc.want, "expiry at age %s against TTL %s", tc.age, tc.ttl)
		})
	}
}

// TestEntry_EffectiveFreshnessAgesInTheCache verifies REQ-EDGE-011 and
// REQ-CACHE-009, which is the reason Entry stores freshness at all: an entry
// written while its data was FRESH goes on ageing in the cache, and once the
// accumulated age passes the freshness TTL it is served as STALE. A cache hit
// must never launder stale data into fresh data.
func TestEntry_EffectiveFreshnessAgesInTheCache(t *testing.T) {
	t.Parallel()

	clk := testutil.NewClock(base)
	e := &Entry{
		StoredAt: clk.Now(),
		CacheTTL: 5 * time.Minute,
		Freshness: domain.Freshness{
			State:      domain.FreshnessFresh,
			Age:        5 * time.Second,
			TTL:        30 * time.Second,
			ObservedAt: base.Add(-5 * time.Second),
			Source:     domain.SourceOperational,
		},
	}

	t.Run("still inside the freshness TTL", func(t *testing.T) {
		f := e.EffectiveFreshness(base.Add(10 * time.Second))
		testutil.Equal(t, f.Age, 15*time.Second, "the stored age plus the time spent cached")
		testutil.Equal(t, f.State, domain.FreshnessFresh, "15s against a 30s TTL is still fresh")
		testutil.Equal(t, f.EvaluatedAt, base.Add(10*time.Second), "the verdict records when it was made")
		testutil.Equal(t, f.ObservedAt, base.Add(-5*time.Second), "the source observation instant is untouched")
	})

	t.Run("past the freshness TTL while still inside the cache TTL", func(t *testing.T) {
		now := base.Add(40 * time.Second)
		testutil.False(t, e.Expired(now), "the entry is still cacheable: its cache TTL is five minutes")
		f := e.EffectiveFreshness(now)
		testutil.Equal(t, f.Age, 45*time.Second, "accumulated age")
		testutil.Equal(t, f.State, domain.FreshnessStale,
			"a reusable cache entry can still hold stale data, and must say so")
	})

	t.Run("the stored entry is not mutated by being read", func(t *testing.T) {
		_ = e.EffectiveFreshness(base.Add(time.Hour))
		testutil.Equal(t, e.Freshness.Age, 5*time.Second, "the entry still carries the age it was born with")
		testutil.Equal(t, e.Freshness.State, domain.FreshnessFresh, "and the state it was born with")
	})
}

// TestEntry_EffectiveFreshnessKeepsUnknownUnknown verifies REQ-TTL-006 as it
// applies to the cache: UNKNOWN means the age could never be established, and
// the router handles it by policy. A cache hit must not upgrade that to FRESH
// on its own authority (REQ-CACHE-009).
func TestEntry_EffectiveFreshnessKeepsUnknownUnknown(t *testing.T) {
	t.Parallel()

	e := &Entry{
		StoredAt:  base,
		CacheTTL:  time.Minute,
		Freshness: domain.Freshness{State: domain.FreshnessUnknown, TTL: 30 * time.Second},
	}

	f := e.EffectiveFreshness(base.Add(10 * time.Second))
	testutil.Equal(t, f.State, domain.FreshnessUnknown,
		"an entry whose freshness was never established does not become fresh by being cached")
	testutil.Equal(t, f.EvaluatedAt, base.Add(10*time.Second), "the evaluation instant is still recorded")

	t.Run("with no freshness TTL nothing is re-judged", func(t *testing.T) {
		t.Parallel()
		noTTL := &Entry{
			StoredAt:  base,
			CacheTTL:  time.Minute,
			Freshness: domain.Freshness{State: domain.FreshnessUnknown},
		}
		got := noTTL.EffectiveFreshness(base.Add(time.Hour))
		testutil.Equal(t, got.State, domain.FreshnessUnknown,
			"without a TTL there is nothing to compare the age against")
	})
}
