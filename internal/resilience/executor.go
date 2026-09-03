package resilience

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// Executor composes the four mechanisms into the single call wrapper the
// adapters use. Order matters and is not arbitrary:
//
//		bulkhead -> breaker -> timeout -> retry(attempt -> timeout)
//
//	  - The bulkhead is outermost so a saturated source is rejected before it can
//	    consume a breaker slot or a goroutine.
//	  - The breaker is next so an open circuit costs nothing.
//	  - The timeout wraps the whole retried operation, so retries share one
//	    deadline rather than each getting a fresh one.
//	  - Each attempt may additionally carry its own shorter timeout.
//
// Traceability: REQ-RES-001, REQ-RES-002, REQ-RES-003, REQ-RES-005.
type Executor struct {
	Name     string
	Breaker  *Breaker
	Retrier  *Retrier
	Bulkhead *Bulkhead

	timeout time.Duration
	// onCall reports every completed call for latency metrics.
	onCall func(name string, d time.Duration, err error)
}

// ExecutorOption customises an Executor.
type ExecutorOption func(*Executor)

// WithCallHook registers a per-call observer.
func WithCallHook(fn func(name string, d time.Duration, err error)) ExecutorOption {
	return func(e *Executor) { e.onCall = fn }
}

// NewExecutor builds an Executor for one data source from its configuration.
func NewExecutor(name string, cfg config.SourceCommon, hooks Hooks, opts ...ExecutorOption) *Executor {
	e := &Executor{
		Name:    name,
		timeout: cfg.CallTimeout.D(),
		Breaker: NewBreaker(name, cfg.Breaker,
			WithTransitionHook(hooks.OnBreakerTransition)),
		Retrier: NewRetrier(cfg.Retry,
			WithRetryHook(func(attempt int, delay time.Duration, err error) {
				hooks.OnRetry(name, attempt, delay, err)
			})),
		Bulkhead: NewBulkhead(name, cfg.Bulkhead, hooks.OnBulkheadChange),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Hooks lets the caller observe resilience events without this package
// importing the observability package (which would invert the dependency).
type Hooks struct {
	OnBreakerTransition func(name string, from, to State)
	OnRetry             func(name string, attempt int, delay time.Duration, err error)
	OnBulkheadChange    func(name string, delta int64)
}

// NoopHooks returns hooks that do nothing.
func NoopHooks() Hooks {
	return Hooks{
		OnBreakerTransition: func(string, State, State) {},
		OnRetry:             func(string, int, time.Duration, error) {},
		OnBulkheadChange:    func(string, int64) {},
	}
}

// Call runs fn under the full resilience stack.
//
// timeoutOverride, when non-zero, replaces the source's default call timeout.
// The router uses it to give a cheap endpoint a tighter budget than the source
// default without reconfiguring the source.
func (e *Executor) Call(ctx context.Context, idempotent bool, timeoutOverride time.Duration, fn func(context.Context) error) error {
	start := time.Now()
	err := e.call(ctx, idempotent, timeoutOverride, fn)
	if e.onCall != nil {
		e.onCall(e.Name, time.Since(start), err)
	}
	return err
}

func (e *Executor) call(ctx context.Context, idempotent bool, timeoutOverride time.Duration, fn func(context.Context) error) error {
	release, err := e.Bulkhead.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	if err := e.Breaker.Allow(); err != nil {
		return err
	}

	timeout := e.timeout
	if timeoutOverride > 0 {
		timeout = timeoutOverride
	}
	// Never extend beyond what the caller already allows: the request budget
	// is authoritative, a per-source timeout may only shorten it.
	callCtx, cancel := withBoundedTimeout(ctx, timeout)
	defer cancel()

	err = e.Retrier.Do(callCtx, idempotent, fn)

	// Only report evidence about the source's health. A client fault is
	// evidence about neither, so it is not recorded -- but it must still return
	// the half-open probe slot that Allow took.
	if err != nil && isClientFault(err) {
		e.Breaker.Abstain()
	} else {
		e.Breaker.Record(err)
	}
	return err
}

// Healthy reports whether the source is currently usable. The router calls
// this before selecting a source, so that a known-open circuit produces a
// routing decision rather than a failed call (REQ-RT-007).
func (e *Executor) Healthy() bool {
	if e == nil {
		return false
	}
	if e.Breaker.State() == StateOpen {
		return false
	}
	return !e.Bulkhead.Saturated()
}

// HealthDetail returns a short machine-readable reason, for error payloads.
func (e *Executor) HealthDetail() string {
	if e == nil {
		return "UNCONFIGURED"
	}
	switch e.Breaker.State() {
	case StateOpen:
		return "CIRCUIT_OPEN"
	case StateHalfOpen:
		if e.Bulkhead.Saturated() {
			return "SATURATED"
		}
		return "CIRCUIT_HALF_OPEN"
	}
	if e.Bulkhead.Saturated() {
		return "SATURATED"
	}
	return "HEALTHY"
}

// withBoundedTimeout applies d, but never past an existing deadline.
func withBoundedTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining < d {
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, d)
}

// ClassifyTimeout converts a context error into a taxonomy error, so adapters
// do not each invent their own wording.
func ClassifyTimeout(source string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errs.CodeOf(err) == errs.CodeUpstreamTimeout:
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeUpstreamTimeout, "data source did not respond within the allotted time", err).
			WithSource(source)
	default:
		return err
	}
}
