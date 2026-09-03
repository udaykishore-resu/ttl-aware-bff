// Package cache implements the BFF's response cache.
//
// The single most important idea in this package is stated in its type names:
// a cache TTL is not a freshness TTL. The cache TTL bounds how long the BFF
// may reuse a computed answer. The freshness TTL bounds how old the underlying
// source data may be for a routing decision. They are configured separately,
// validated against each other at load time, and reported separately in the
// response envelope.
//
// A cached entry therefore carries the freshness it was born with, and serving
// it re-reports that freshness with the age it has accumulated since. A cache
// hit never makes stale data look fresh (REQ-CACHE-001, REQ-EDGE-011).
//
// Traceability: REQ-CACHE-001..REQ-CACHE-010, REQ-MT-003, REQ-EDGE-012.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
)

// ErrMiss is returned by Get when the key is absent.
var ErrMiss = errors.New("cache: miss")

// Entry is what the cache stores. It is deliberately self-describing: given an
// entry and the current time, everything the response envelope needs about
// age, freshness and provenance can be reconstructed without consulting the
// source again.
type Entry struct {
	// Payload is the marshalled canonical response body.
	Payload []byte `json:"p"`
	// StoredAt is when the BFF wrote the entry.
	StoredAt time.Time `json:"s"`
	// CacheTTL is the lifetime this entry was written with.
	CacheTTL time.Duration `json:"c"`
	// Freshness is the source freshness evaluated when the entry was created.
	// Serving the entry re-reports this with the accumulated age added.
	Freshness domain.Freshness `json:"f"`
	// Sources records which data sources contributed.
	Sources []domain.SourceKind `json:"src"`
	// Provenance records which source supplied each canonical field. It is
	// stored rather than recomputed so that a cache hit explains itself exactly
	// as the original answer did; a response whose provenance vanishes on a
	// cache hit is worse than one that never had any.
	Provenance map[string]domain.SourceKind `json:"prov,omitempty"`
	// Warnings are the non-fatal conditions attached to the cached answer.
	Warnings []domain.Warning `json:"warn,omitempty"`
	// Partial records that the cached answer was assembled without one of the
	// sources it wanted.
	Partial bool `json:"part,omitempty"`
	// RoutingRule is the rule that produced the cached answer.
	RoutingRule string `json:"rr"`
	// Degraded marks an entry produced by a degraded path. Such an entry is
	// cached with a shortened TTL so the degradation does not outlive the
	// outage that caused it.
	Degraded bool `json:"d"`
	// Negative marks a cached "not found".
	Negative bool `json:"n"`
	// SchemaVersion guards against reading entries written by an older build
	// with a different payload shape.
	SchemaVersion int `json:"v"`
}

// EntrySchemaVersion is bumped whenever Entry's payload shape changes. An
// entry with a different version is treated as a miss rather than decoded,
// which makes rolling deploys safe without flushing Redis.
const EntrySchemaVersion = 1

// Age returns how long the entry has been in the cache.
func (e *Entry) Age(now time.Time) time.Duration { return now.Sub(e.StoredAt) }

// Expired reports whether the entry has outlived its cache TTL.
func (e *Entry) Expired(now time.Time) bool {
	if e.CacheTTL <= 0 {
		return true
	}
	return e.Age(now) >= e.CacheTTL
}

// EffectiveFreshness returns the freshness of the cached data as of now: the
// age it had when stored, plus the time it has spent in the cache. This is the
// mechanism that stops a cache hit from laundering stale data into fresh data.
func (e *Entry) EffectiveFreshness(now time.Time) domain.Freshness {
	f := e.Freshness
	f.Age += e.Age(now)
	f.EvaluatedAt = now
	// An entry whose freshness was never established stays UNKNOWN. Ageing it
	// into STALE would look conservative but is actually a loss of information:
	// the router treats UNKNOWN and STALE differently on purpose (REQ-TTL-006),
	// and a cache hit must not manufacture a verdict the original read did not
	// produce.
	if f.TTL > 0 && f.State != domain.FreshnessUnknown {
		if f.Age > f.TTL {
			f.State = domain.FreshnessStale
		} else {
			f.State = domain.FreshnessFresh
		}
	}
	return f
}

// Cache is the storage port. Implementations must be safe for concurrent use.
type Cache interface {
	// Get returns the entry for key, or ErrMiss.
	Get(ctx context.Context, key string) (*Entry, error)
	// Set stores an entry with the given TTL.
	Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error
	// Delete removes a key.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every key under a prefix. Used for invalidation.
	DeletePrefix(ctx context.Context, prefix string) error
	// Name identifies the backend for metrics.
	Name() string
	// Close releases resources.
	Close() error
}

// ---------------------------------------------------------------------------
// Key design
// ---------------------------------------------------------------------------

// Key is a structured cache key. Building keys through this type rather than
// with fmt.Sprintf is what makes it impossible to forget the tenant, which is
// the single most damaging cache bug a multi-tenant BFF can have
// (REQ-MT-003).
type Key struct {
	Prefix      string
	TenantID    string
	RequestType string
	ResourceID  string
	// Sub distinguishes sub-resources, e.g. an execution id.
	Sub string
	// Variant captures anything else that changes the response body:
	// pagination, field selection, the caller's permission set.
	Variant map[string]string
}

// String renders the key. Layout:
//
//	<prefix>:<schema>:t=<tenant>:rt=<requestType>:r=<resource>[:s=<sub>][:v=<hash>]
//
// The tenant segment comes first after the prefix so that a DeletePrefix on
// "prefix:schema:t=acme" evicts exactly one tenant and nothing else.
func (k Key) String() string {
	var sb strings.Builder
	sb.Grow(96)
	sb.WriteString(k.Prefix)
	sb.WriteString(":e")
	sb.WriteString(itoa(EntrySchemaVersion))
	sb.WriteString(":t=")
	sb.WriteString(sanitizeSegment(k.TenantID))
	sb.WriteString(":rt=")
	sb.WriteString(sanitizeSegment(k.RequestType))
	sb.WriteString(":r=")
	sb.WriteString(sanitizeSegment(k.ResourceID))
	if k.Sub != "" {
		sb.WriteString(":s=")
		sb.WriteString(sanitizeSegment(k.Sub))
	}
	if len(k.Variant) > 0 {
		sb.WriteString(":v=")
		sb.WriteString(hashVariant(k.Variant))
	}
	return sb.String()
}

// TenantPrefix returns the prefix covering every key of one tenant.
//
// The trailing delimiter is load-bearing. Without it the prefix
// "bff:v1:e1:t=acme" also matches "bff:v1:e1:t=acme2:...", so flushing tenant
// "acme" would silently empty tenant "acme2" as well. The key layout always
// puts ":" after the tenant segment, so including it makes the prefix exact.
func TenantPrefix(prefix, tenantID string) string {
	return prefix + ":e" + itoa(EntrySchemaVersion) + ":t=" + sanitizeSegment(tenantID) + ":"
}

// There is deliberately no ResourcePrefix helper. The request type sits between
// the tenant and the resource in the key layout, so "every view of one
// resource" is not a prefix at all -- it is a set of exact keys, which is what
// ResourceKeysForTypes returns. A helper that looked like a prefix would invite
// a wildcard scan that either matches nothing or matches too much.

// ResourceKeysForTypes returns the exact keys to evict for one resource across
// the supplied request types. Exact keys beat wildcard scans: KEYS and SCAN on
// a busy Redis are how a cache invalidation becomes an outage.
func ResourceKeysForTypes(prefix, tenantID, resourceID string, requestTypes []string) []string {
	out := make([]string, 0, len(requestTypes))
	for _, rt := range requestTypes {
		out = append(out, Key{Prefix: prefix, TenantID: tenantID, RequestType: rt, ResourceID: resourceID}.String())
	}
	return out
}

func hashVariant(v map[string]string) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(v[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sanitizeSegment keeps a key segment free of the delimiter and of characters
// that would let a crafted resource id impersonate another tenant's key.
// emptySegment stands in for an absent segment value. It uses a character
// OUTSIDE the safe set, so it cannot collide with a real id: "-" would be
// produced both by an empty string and by a tenant literally named "-".
const emptySegment = "~"

func sanitizeSegment(s string) string {
	if s == "" {
		return emptySegment
	}
	var sb strings.Builder
	sb.Grow(len(s))
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			safe = false
		}
	}
	if !safe {
		// Anything unexpected collapses to a hash rather than being dropped,
		// so two different odd ids cannot collide onto one key.
		sum := sha256.Sum256([]byte(s))
		return "h_" + hex.EncodeToString(sum[:])[:24]
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
