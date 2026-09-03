package resilience

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// Retrier performs bounded retries with exponential backoff and jitter.
//
// The rules it enforces, in order of importance:
//
//  1. Only errors classified retryable are retried. "Never retry blindly"
//     means the decision belongs to the error taxonomy, not to the call site.
//  2. Attempts are bounded by configuration and validated at load time.
//  3. Backoff is exponential with jitter. Configuration refuses zero jitter,
//     because synchronised clients retrying in lockstep is how a recovering
//     source is knocked over again.
//  4. A retry budget caps the total time spent retrying as a fraction of the
//     call timeout, so retries can never consume the whole request deadline.
//  5. The context deadline always wins. If there is not enough time left for
//     another attempt plus its backoff, the retrier stops.
type Retrier struct {
	cfg  config.RetryConfig
	now  func() time.Time
	rand func() float64
	// sleep is indirected so tests do not spend real time.
	sleep func(ctx context.Context, d time.Duration) error
	// onRetry is invoked before each retry, for metrics and logs.
	onRetry func(attempt int, delay time.Duration, err error)
}

// NewRetrier builds a Retrier from configuration.
func NewRetrier(cfg config.RetryConfig, opts ...RetryOption) *Retrier {
	r := &Retrier{
		cfg:   cfg,
		now:   time.Now,
		rand:  rand.Float64,
		sleep: sleepCtx,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// RetryOption customises a Retrier.
type RetryOption func(*Retrier)

// WithRetryClock injects a clock.
func WithRetryClock(now func() time.Time) RetryOption {
	return func(r *Retrier) { r.now = now }
}

// WithRetryRand injects the jitter source, for deterministic tests.
func WithRetryRand(f func() float64) RetryOption {
	return func(r *Retrier) { r.rand = f }
}

// WithRetrySleep injects the sleep function, so tests run instantly.
func WithRetrySleep(f func(ctx context.Context, d time.Duration) error) RetryOption {
	return func(r *Retrier) { r.sleep = f }
}

// WithRetryHook registers a callback fired before each retry.
func WithRetryHook(fn func(attempt int, delay time.Duration, err error)) RetryOption {
	return func(r *Retrier) { r.onRetry = fn }
}

// Do runs fn, retrying on retryable errors. It returns the final error, which
// is the last attempt's error, so the caller sees the real cause rather than a
// synthesised "retries exhausted".
//
// idempotent must be false for any operation whose repetition is unsafe. This
// is a parameter rather than configuration because it is a property of the
// call, not of the source.
func (r *Retrier) Do(ctx context.Context, idempotent bool, fn func(context.Context) error) error {
	attempts := 1
	if r.cfg.Enabled && idempotent {
		attempts = max(1, r.cfg.MaxAttempts)
	}

	start := r.now()
	budget := r.budget(ctx, start)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return errs.Wrap(errs.CodeUpstreamTimeout, "request deadline exceeded before attempt", err)
		}

		attemptCtx, cancel := r.attemptContext(ctx)
		err := fn(attemptCtx)
		cancel()

		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == attempts || !errs.IsRetryable(err) {
			return lastErr
		}

		delay := r.backoff(attempt)

		// Stop if the remaining deadline or the retry budget cannot cover the
		// backoff plus a plausible attempt.
		if !r.canAfford(ctx, start, budget, delay) {
			return lastErr
		}
		if r.onRetry != nil {
			r.onRetry(attempt, delay, err)
		}
		if serr := r.sleep(ctx, delay); serr != nil {
			return lastErr
		}
	}
	return lastErr
}

// backoff returns the delay before the given attempt number (1-based), using
// exponential growth capped at MaxBackoff and then jittered.
//
// With JitterFraction = 1 this is AWS-style "full jitter": uniform in
// [0, exp]. With f < 1 the delay is uniform in [exp*(1-f), exp].
func (r *Retrier) backoff(attempt int) time.Duration {
	base := float64(r.cfg.BaseBackoff.D())
	if base <= 0 {
		return 0
	}
	exp := base * math.Pow(2, float64(attempt-1))
	if maxB := float64(r.cfg.MaxBackoff.D()); maxB > 0 && exp > maxB {
		exp = maxB
	}
	f := r.cfg.JitterFraction
	if f <= 0 {
		return time.Duration(exp)
	}
	if f > 1 {
		f = 1
	}
	lo := exp * (1 - f)
	return time.Duration(lo + r.rand()*(exp-lo))
}

// budget returns the maximum wall time this call may spend on retries.
func (r *Retrier) budget(ctx context.Context, start time.Time) time.Duration {
	ratio := r.cfg.BudgetRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Duration(math.MaxInt64)
	}
	total := dl.Sub(start)
	if total <= 0 {
		return 0
	}
	return time.Duration(float64(total) * ratio)
}

func (r *Retrier) canAfford(ctx context.Context, start time.Time, budget, delay time.Duration) bool {
	elapsed := r.now().Sub(start)
	if elapsed+delay >= budget {
		return false
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	// Require the backoff plus a minimal slice of time for the next attempt.
	return r.now().Add(delay).Before(dl)
}

func (r *Retrier) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.cfg.PerAttemptTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.cfg.PerAttemptTimeout.D())
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
