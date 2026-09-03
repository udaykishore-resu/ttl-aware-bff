// Package errs defines the typed error taxonomy shared by every layer of the
// BFF. Adapters classify transport failures into these types; the application
// layer decides degradation from them; the API layer maps them to HTTP.
//
// The taxonomy exists so that no layer has to string-match another layer's
// errors, and so that "is this retryable?" is answered once, in one place.
//
// Traceability: REQ-ERR-001..REQ-ERR-012, REQ-RES-004.
package errs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Code is the stable, machine-readable error identifier surfaced to the UI.
// The set is closed: adding a member is an API change.
type Code string

const (
	CodeInvalidRequest         Code = "INVALID_REQUEST"
	CodeUnauthenticated        Code = "UNAUTHENTICATED"
	CodeForbidden              Code = "FORBIDDEN"
	CodeTenantMismatch         Code = "TENANT_MISMATCH"
	CodeNotFound               Code = "NOT_FOUND"
	CodeRateLimited            Code = "RATE_LIMITED"
	CodeRequestTooLarge        Code = "REQUEST_TOO_LARGE"
	CodeUpstreamTimeout        Code = "UPSTREAM_TIMEOUT"
	CodeUpstreamUnavailable    Code = "UPSTREAM_UNAVAILABLE"
	CodeUpstreamInvalidPayload Code = "UPSTREAM_INVALID_RESPONSE"
	CodeSchemaVersionMismatch  Code = "SCHEMA_VERSION_MISMATCH"
	CodeNoSourceAvailable      Code = "NO_SOURCE_AVAILABLE"
	CodeInternal               Code = "INTERNAL"
)

// String renders the code, so it can be used directly as a span status or a
// metric attribute without a conversion at every call site.
func (c Code) String() string { return string(c) }

// Class groups codes by how the caller should react. It is derived from the
// code, never set independently, so classification cannot drift.
type Class int

const (
	// ClassClient means the caller sent something wrong. Never retry.
	ClassClient Class = iota
	// ClassTransient means the same request may succeed later. Retry is
	// permitted subject to the retry budget and idempotency rules.
	ClassTransient
	// ClassDegradable means the request failed but a lower-fidelity answer
	// (stale or partial) may still be servable.
	ClassDegradable
	// ClassTerminal means retrying cannot help and no degraded answer exists.
	ClassTerminal
)

// httpStatusByCode is the single mapping from taxonomy to transport.
var httpStatusByCode = map[Code]int{
	CodeInvalidRequest:         http.StatusBadRequest,
	CodeUnauthenticated:        http.StatusUnauthorized,
	CodeForbidden:              http.StatusForbidden,
	CodeTenantMismatch:         http.StatusForbidden,
	CodeNotFound:               http.StatusNotFound,
	CodeRateLimited:            http.StatusTooManyRequests,
	CodeRequestTooLarge:        http.StatusRequestEntityTooLarge,
	CodeUpstreamTimeout:        http.StatusGatewayTimeout,
	CodeUpstreamUnavailable:    http.StatusServiceUnavailable,
	CodeUpstreamInvalidPayload: http.StatusBadGateway,
	CodeSchemaVersionMismatch:  http.StatusBadGateway,
	CodeNoSourceAvailable:      http.StatusServiceUnavailable,
	CodeInternal:               http.StatusInternalServerError,
}

var classByCode = map[Code]Class{
	CodeInvalidRequest:         ClassClient,
	CodeUnauthenticated:        ClassClient,
	CodeForbidden:              ClassClient,
	CodeTenantMismatch:         ClassClient,
	CodeRequestTooLarge:        ClassClient,
	CodeNotFound:               ClassTerminal,
	CodeRateLimited:            ClassTransient,
	CodeUpstreamTimeout:        ClassDegradable,
	CodeUpstreamUnavailable:    ClassDegradable,
	CodeUpstreamInvalidPayload: ClassTerminal,
	CodeSchemaVersionMismatch:  ClassTerminal,
	CodeNoSourceAvailable:      ClassTerminal,
	CodeInternal:               ClassTerminal,
}

// Error is the canonical error value carried across layer boundaries.
type Error struct {
	Code Code
	// Message is safe to expose to the UI. It must never contain upstream
	// hostnames, credentials, schema fragments or stack detail (REQ-SEC-009).
	Message string
	// Source names the data source that produced the failure, when applicable.
	Source string
	// Op is the internal operation name, used for logs and metrics only.
	Op string
	// Retryable is derived from Code but may be narrowed by the adapter (for
	// example, a non-idempotent call is never retryable regardless of code).
	Retryable bool
	// Cause is the wrapped underlying error. Never serialised to the UI.
	Cause error
	// Details carries structured, non-sensitive context for logs.
	Details map[string]any
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (op=%s source=%s): %v", e.Code, e.Message, e.Op, e.Source, e.Cause)
	}
	return fmt.Sprintf("%s: %s (op=%s source=%s)", e.Code, e.Message, e.Op, e.Source)
}

func (e *Error) Unwrap() error { return e.Cause }

// Class returns the reaction class implied by the code.
func (e *Error) Class() Class {
	if e == nil {
		return ClassTerminal
	}
	if c, ok := classByCode[e.Code]; ok {
		return c
	}
	return ClassTerminal
}

// HTTPStatus returns the transport status for this error.
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	if s, ok := httpStatusByCode[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// New builds an Error, deriving Retryable from the code.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg, Retryable: defaultRetryable(code)}
}

// Wrap builds an Error around an existing cause.
func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause, Retryable: defaultRetryable(code)}
}

// WithSource returns a copy tagged with the originating data source.
func (e *Error) WithSource(src string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Source = src
	return &c
}

// WithOp returns a copy tagged with the internal operation name.
func (e *Error) WithOp(op string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Op = op
	return &c
}

// WithDetail attaches structured, log-only context.
func (e *Error) WithDetail(k string, v any) *Error {
	if e == nil {
		return nil
	}
	c := *e
	if c.Details == nil {
		c.Details = map[string]any{}
	} else {
		cp := make(map[string]any, len(c.Details)+1)
		for dk, dv := range c.Details {
			cp[dk] = dv
		}
		c.Details = cp
	}
	c.Details[k] = v
	return &c
}

// NotRetryable returns a copy with retry explicitly forbidden. Adapters use
// this for non-idempotent operations: "never retry blindly" (REQ-RES-004).
func (e *Error) NotRetryable() *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Retryable = false
	return &c
}

func defaultRetryable(code Code) bool {
	switch code {
	// NO_SOURCE_AVAILABLE is terminal for THIS attempt -- there is no other
	// source to try and no degraded answer to serve -- but the condition that
	// produced it is an outage, and outages end. The response advertises
	// Retry-After, so the retryable hint must agree with it.
	case CodeUpstreamTimeout, CodeUpstreamUnavailable, CodeRateLimited, CodeNoSourceAvailable:
		return true
	default:
		return false
	}
}

// As extracts an *Error from an error chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the taxonomy code for any error, defaulting to INTERNAL.
// Context cancellation and deadline are mapped explicitly so that callers do
// not have to special-case them everywhere.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if e, ok := As(err); ok {
		return e.Code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return CodeUpstreamTimeout
	case errors.Is(err, context.Canceled):
		return CodeUpstreamUnavailable
	default:
		return CodeInternal
	}
}

// IsRetryable reports whether a retry may be attempted for this error.
// A cancelled context is never retryable: the caller has gone away.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if e, ok := As(err); ok {
		return e.Retryable
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// IsDegradable reports whether a lower-fidelity answer may be served instead
// of failing the request outright (REQ-RES-007).
func IsDegradable(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := As(err); ok {
		return e.Class() == ClassDegradable
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// SourceUnusable reports whether this error means "the source that produced it
// cannot serve this request, but a different source might".
//
// It is deliberately wider than IsDegradable. A schema-version mismatch is
// terminal -- retrying cannot help, and no amount of waiting will make an
// incompatible contract compatible -- but it is still a reason to try the other
// source rather than to fail, in exactly the way a timeout is. Conversely a
// NOT_FOUND is neither: asking a second source about a resource the first
// authoritatively does not have would turn a correct answer into a wrong one
// (REQ-RES-006).
func SourceUnusable(err error) bool {
	switch CodeOf(err) {
	case CodeUpstreamTimeout, CodeUpstreamUnavailable, CodeUpstreamInvalidPayload, CodeSchemaVersionMismatch:
		return true
	default:
		return false
	}
}

// IsNotFound is a convenience used by handlers and the aggregator.
func IsNotFound(err error) bool { return CodeOf(err) == CodeNotFound }

// HTTPStatusOf returns the transport status for any error.
func HTTPStatusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if e, ok := As(err); ok {
		return e.HTTPStatus()
	}
	if s, ok := httpStatusByCode[CodeOf(err)]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Sentinel errors for conditions that carry no additional context.
var (
	ErrNotFound        = New(CodeNotFound, "resource not found")
	ErrNoSource        = New(CodeNoSourceAvailable, "no data source can satisfy this request")
	ErrCircuitOpen     = New(CodeUpstreamUnavailable, "data source temporarily unavailable")
	ErrBulkheadFull    = New(CodeUpstreamUnavailable, "data source concurrency limit reached")
	ErrRateLimited     = New(CodeRateLimited, "request rate limit exceeded")
	ErrTenantMismatch  = New(CodeTenantMismatch, "tenant context does not match credentials")
	ErrUnauthenticated = New(CodeUnauthenticated, "missing or invalid credentials")
)
