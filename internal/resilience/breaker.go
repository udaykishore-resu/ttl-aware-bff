// Package resilience contains the mechanisms that keep a slow or failing data
// source from becoming a slow or failing BFF: bounded timeouts, bounded
// retries, circuit breakers, bulkheads and rate limiters.
//
// Everything here is deliberately dependency-free and configuration-driven.
// No policy decision is embedded in the mechanism.
//
// Traceability: REQ-RES-001..REQ-RES-012.
package resilience

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// State is the circuit breaker's state.
type State int

const (
	// StateClosed passes calls through and counts outcomes.
	StateClosed State = iota
	// StateOpen rejects calls immediately.
	StateOpen
	// StateHalfOpen admits a bounded number of probe calls.
	StateHalfOpen
)

// String renders the state for metrics and logs.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	}
	return "unknown"
}

// Breaker is a three-state circuit breaker over a rolling time window.
//
// Design choices worth stating:
//
//   - The window is a ring of fixed-width buckets rather than a single pair of
//     counters, so a burst of failures ages out gradually instead of being
//     cleared wholesale. A breaker that forgets everything on a tick boundary
//     flaps.
//   - A minimum request count gates the failure ratio. Without it, the first
//     failed call after a quiet period trips the breaker at a 100% ratio.
//   - Half-open admits a bounded number of concurrent probes and requires
//     several consecutive successes before closing, so one lucky call does not
//     re-admit full traffic to a source that is still unwell.
type Breaker struct {
	name string
	cfg  config.BreakerConfig
	now  func() time.Time

	// onTransition is invoked outside the lock for metrics and logging.
	onTransition func(name string, from, to State)

	mu        sync.Mutex
	state     State
	buckets   []bucket
	bucketDur time.Duration
	startedAt time.Time

	openedAt      time.Time
	halfOpenCalls int
	halfOpenOK    int
}

type bucket struct {
	index    int64
	success  int
	failure  int
	rejected int
}

const bucketsPerWindow = 10

// NewBreaker builds a breaker. A disabled configuration yields a breaker that
// always allows calls, so call sites never need a nil check.
func NewBreaker(name string, cfg config.BreakerConfig, opts ...BreakerOption) *Breaker {
	b := &Breaker{
		name:      name,
		cfg:       cfg,
		now:       time.Now,
		state:     StateClosed,
		buckets:   make([]bucket, bucketsPerWindow),
		bucketDur: cfg.Window.D() / bucketsPerWindow,
	}
	if b.bucketDur <= 0 {
		b.bucketDur = time.Second
	}
	for _, o := range opts {
		o(b)
	}
	b.startedAt = b.now()
	for i := range b.buckets {
		b.buckets[i].index = -1
	}
	return b
}

// BreakerOption customises a Breaker.
type BreakerOption func(*Breaker)

// WithBreakerClock injects a clock, for deterministic tests.
func WithBreakerClock(now func() time.Time) BreakerOption {
	return func(b *Breaker) { b.now = now }
}

// WithTransitionHook registers a callback fired on every state change.
func WithTransitionHook(fn func(name string, from, to State)) BreakerOption {
	return func(b *Breaker) { b.onTransition = fn }
}

// Name returns the breaker's identity, used as a metric attribute.
func (b *Breaker) Name() string { return b.name }

// State returns the current state, advancing the open->half-open timer if due.
func (b *Breaker) State() State {
	b.mu.Lock()
	prev := b.state
	st := b.currentLocked()
	b.mu.Unlock()
	if prev != st {
		b.fire(prev, st)
	}
	return st
}

// Allow reports whether a call may proceed. When it returns false the caller
// must not touch the source; the returned error is already classified.
func (b *Breaker) Allow() error {
	if !b.cfg.Enabled {
		return nil
	}
	var from, to State
	b.mu.Lock()
	// The previous state must be captured BEFORE currentLocked runs, because
	// currentLocked may itself perform the open -> half-open transition and
	// return the new state. Comparing against its return value would make that
	// transition invisible to metrics, and a dashboard could never tell that a
	// breaker had started probing.
	from = b.state
	st := b.currentLocked()
	switch st {
	case StateOpen:
		b.recordLocked(func(bk *bucket) { bk.rejected++ })
		b.mu.Unlock()
		return errs.ErrCircuitOpen.WithSource(b.name).WithOp("breaker.allow")
	case StateHalfOpen:
		if b.halfOpenCalls >= b.cfg.HalfOpenMaxCalls {
			b.recordLocked(func(bk *bucket) { bk.rejected++ })
			b.mu.Unlock()
			return errs.ErrCircuitOpen.WithSource(b.name).WithOp("breaker.half_open_saturated")
		}
		b.halfOpenCalls++
	}
	to = b.state
	b.mu.Unlock()
	if from != to {
		b.fire(from, to)
	}
	return nil
}

// Record reports a call's outcome. err == nil means success. Errors that are
// the caller's fault (invalid request, not found) must NOT be reported here:
// they say nothing about the source's health. The adapters filter them out.
func (b *Breaker) Record(err error) {
	if !b.cfg.Enabled {
		return
	}
	var from, to State
	b.mu.Lock()
	from = b.state
	st := b.currentLocked()
	if err == nil {
		b.recordLocked(func(bk *bucket) { bk.success++ })
		if st == StateHalfOpen {
			b.halfOpenOK++
			b.halfOpenCalls = decr(b.halfOpenCalls)
			if b.halfOpenOK >= b.cfg.HalfOpenSuccesses {
				b.transitionLocked(StateClosed)
			}
		}
	} else {
		b.recordLocked(func(bk *bucket) { bk.failure++ })
		switch st {
		case StateHalfOpen:
			// A single failure during probing re-opens immediately: the source
			// has told us it is still unwell.
			b.halfOpenCalls = decr(b.halfOpenCalls)
			b.transitionLocked(StateOpen)
		case StateClosed:
			if b.shouldTripLocked() {
				b.transitionLocked(StateOpen)
			}
		}
	}
	to = b.state
	b.mu.Unlock()
	if from != to {
		b.fire(from, to)
	}
}

// Abstain releases a probe slot taken by Allow without recording an outcome.
//
// It exists because half-open admission and outcome recording are two separate
// steps: Allow takes a slot, Record gives it back. A call that produces no
// health evidence must still return its slot, or half_open_max_calls client
// faults in a row would hold every probe slot until the next open timeout,
// stalling recovery on a source that was never the problem.
func (b *Breaker) Abstain() {
	if !b.cfg.Enabled {
		return
	}
	b.mu.Lock()
	if b.state == StateHalfOpen {
		b.halfOpenCalls = decr(b.halfOpenCalls)
	}
	b.mu.Unlock()
}

// Do runs fn under the breaker.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := b.Allow(); err != nil {
		return err
	}
	err := fn(ctx)
	// Client-caused failures are not evidence about the source -- in either
	// direction. Recording them as successes would be just as wrong as
	// recording them as failures: a source answering nothing but 404s while it
	// is genuinely down would accumulate "successes", satisfy the half-open
	// threshold and be re-admitted to full traffic.
	if err != nil && isClientFault(err) {
		b.Abstain()
		return err
	}
	b.Record(err)
	return err
}

// Counts returns the current window's tallies, for tests and the admin surface.
func (b *Breaker) Counts() (success, failure, rejected int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := b.bucketIndex(b.now())
	oldest := idx - int64(bucketsPerWindow) + 1
	for i := range b.buckets {
		bk := b.buckets[i]
		if bk.index < oldest || bk.index > idx {
			continue
		}
		success += bk.success
		failure += bk.failure
		rejected += bk.rejected
	}
	return
}

// ---------------------------------------------------------------------------
// internals (all called with b.mu held)
// ---------------------------------------------------------------------------

func (b *Breaker) currentLocked() State {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cfg.OpenTimeout.D() {
		b.transitionLocked(StateHalfOpen)
	}
	return b.state
}

func (b *Breaker) transitionLocked(to State) {
	if b.state == to {
		return
	}
	b.state = to
	switch to {
	case StateOpen:
		b.openedAt = b.now()
		b.halfOpenCalls, b.halfOpenOK = 0, 0
	case StateHalfOpen:
		b.halfOpenCalls, b.halfOpenOK = 0, 0
	case StateClosed:
		b.halfOpenCalls, b.halfOpenOK = 0, 0
		// Clearing the window on close prevents the pre-outage failures from
		// immediately re-tripping the breaker on the first new failure.
		for i := range b.buckets {
			b.buckets[i] = bucket{index: -1}
		}
	}
}

func (b *Breaker) shouldTripLocked() bool {
	var success, failure int
	idx := b.bucketIndex(b.now())
	oldest := idx - int64(bucketsPerWindow) + 1
	for i := range b.buckets {
		bk := b.buckets[i]
		if bk.index < oldest || bk.index > idx {
			continue
		}
		success += bk.success
		failure += bk.failure
	}
	total := success + failure
	if total < b.cfg.MinimumRequests {
		return false
	}
	return float64(failure)/float64(total) >= b.cfg.FailureThreshold
}

func (b *Breaker) recordLocked(fn func(*bucket)) {
	idx := b.bucketIndex(b.now())
	slot := int(idx % int64(bucketsPerWindow))
	if b.buckets[slot].index != idx {
		b.buckets[slot] = bucket{index: idx}
	}
	fn(&b.buckets[slot])
}

// bucketIndex maps an instant to a ring slot.
//
// It clamps at zero. In production b.startedAt carries a monotonic reading and
// the index cannot go negative, but an injected clock that strips that reading
// (time.Now().UTC() is enough) plus a backwards wall-clock step would produce a
// negative index and a slice panic inside a held lock -- a process-killing bug
// that would only ever appear on a machine whose clock was already misbehaving.
func (b *Breaker) bucketIndex(t time.Time) int64 {
	idx := int64(t.Sub(b.startedAt) / b.bucketDur)
	if idx < 0 {
		return 0
	}
	return idx
}

func (b *Breaker) fire(from, to State) {
	if b.onTransition != nil {
		b.onTransition(b.name, from, to)
	}
}

func decr(n int) int {
	if n <= 0 {
		return 0
	}
	return n - 1
}

// isClientFault reports whether an error is evidence about something other
// than the source's health, and so must not count toward opening a circuit.
//
// Two groups qualify. The first is the caller's own fault -- a not-found, a
// malformed request, a denied permission -- which says nothing about whether
// the source is well.
//
// The second is a schema-version mismatch, which is a subtler case worth
// stating plainly: the source is up, fast and answering correctly, and the BFF
// simply does not understand the contract it is speaking. Counting that as a
// health failure would trip the breaker on a perfectly healthy source, quietly
// reroute every request to the slower one, and replace a loud, obvious
// version-incompatibility alert with a vague availability one. The mismatch is
// surfaced through schema_version_mismatch_total and through the call-time
// fallback instead (REQ-EDGE-017).
func isClientFault(err error) bool {
	e, ok := errs.As(err)
	if !ok {
		return false
	}
	switch e.Code {
	case errs.CodeNotFound, errs.CodeInvalidRequest, errs.CodeForbidden,
		errs.CodeUnauthenticated, errs.CodeTenantMismatch,
		errs.CodeSchemaVersionMismatch:
		return true
	}
	return false
}
