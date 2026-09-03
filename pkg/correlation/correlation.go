// Package correlation carries per-request identity through the call graph:
// correlation id, request id, tenant, principal and the deadline budget.
//
// It is deliberately transport-agnostic so that the gRPC adapter, the REST
// adapter, the cache and the logger all read the same values from context
// without importing net/http.
//
// Traceability: REQ-API-006, REQ-MT-002, REQ-OBS-006.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

type ctxKey int

const (
	keyCorrelationID ctxKey = iota
	keyRequestID
	keyTenantID
	keyPrincipal
	keyRoles
	keyStart
)

// HeaderCorrelationID is the wire header used inbound and outbound.
const (
	HeaderCorrelationID = "X-Correlation-ID"
	HeaderRequestID     = "X-Request-ID"
	HeaderTenantID      = "X-Tenant-ID"
	// MetadataCorrelationID is the gRPC metadata key (lower-case by protocol).
	MetadataCorrelationID = "x-correlation-id"
	MetadataTenantID      = "x-tenant-id"
)

// NewID returns a 128-bit hex identifier. It never fails: on entropy failure
// it falls back to a time-derived value rather than aborting the request.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		ns := uint64(time.Now().UnixNano())
		for i := 0; i < 8; i++ {
			b[i] = byte(ns >> (8 * i))
			b[8+i] = byte(ns>>(8*i)) ^ 0xA5
		}
	}
	return hex.EncodeToString(b[:])
}

// SanitizeID accepts a client-supplied correlation id only if it is safe to
// echo back and to use as a log/metric attribute: bounded length, and limited
// to characters that cannot break log formats or header encoding.
// An unacceptable value is replaced rather than rejected, because a bad
// correlation id must never fail an otherwise valid request (REQ-API-006).
func SanitizeID(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 {
		return "", false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return "", false
		}
	}
	return v, true
}

// WithCorrelationID stores the correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyCorrelationID, id)
}

// CorrelationID returns the correlation id, or "" if unset.
func CorrelationID(ctx context.Context) string { return str(ctx, keyCorrelationID) }

// WithRequestID stores the per-hop request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the per-hop request id.
func RequestID(ctx context.Context) string { return str(ctx, keyRequestID) }

// WithTenantID stores the resolved tenant. This is the value every downstream
// component must use; nothing may re-read the raw header (REQ-MT-002).
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTenantID, id)
}

// TenantID returns the resolved tenant, or "" if the request is unauthenticated.
func TenantID(ctx context.Context) string { return str(ctx, keyTenantID) }

// WithPrincipal stores the authenticated subject.
func WithPrincipal(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, keyPrincipal, sub)
}

// Principal returns the authenticated subject.
func Principal(ctx context.Context) string { return str(ctx, keyPrincipal) }

// WithRoles stores the authorised roles.
func WithRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, keyRoles, roles)
}

// Roles returns the authorised roles.
func Roles(ctx context.Context) []string {
	if v, ok := ctx.Value(keyRoles).([]string); ok {
		return v
	}
	return nil
}

// WithStart records the instant the request entered the BFF, used to compute
// the remaining latency budget for downstream fan-out.
func WithStart(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, keyStart, t)
}

// Start returns the request start instant, or the zero time.
func Start(ctx context.Context) time.Time {
	if v, ok := ctx.Value(keyStart).(time.Time); ok {
		return v
	}
	return time.Time{}
}

// Elapsed returns how long the request has been in the BFF.
func Elapsed(ctx context.Context, now time.Time) time.Duration {
	s := Start(ctx)
	if s.IsZero() {
		return 0
	}
	return now.Sub(s)
}

func str(ctx context.Context, k ctxKey) string {
	if v, ok := ctx.Value(k).(string); ok {
		return v
	}
	return ""
}
