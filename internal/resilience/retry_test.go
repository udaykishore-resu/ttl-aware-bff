package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// retryCfg is the shared starting point for the retry tests.
func retryCfg() config.RetryConfig {
	return config.RetryConfig{
		Enabled:        true,
		MaxAttempts:    3,
		BaseBackoff:    config.Duration(20 * time.Millisecond),
		MaxBackoff:     config.Duration(400 * time.Millisecond),
		JitterFraction: 1.0,
		BudgetRatio:    0.6,
	}
}

// recorder captures what a Retrier did without spending any real time.
type recorder struct {
	slept   []time.Duration
	retries []time.Duration
	clk     *testutil.Clock
	// onSleep, when set, runs instead of advancing the clock.
	onSleep func(ctx context.Context, d time.Duration) error
}

// newRecorder anchors its manually advanced clock at the current instant. The
// anchor matters because context deadlines are real, so a test that mixes a
// context deadline with the injected clock needs the two to start together; all
// elapsed-time arithmetic still comes from the fake clock alone.
func newRecorder() *recorder {
	return &recorder{clk: testutil.NewClock(time.Now())}
}

// sleep advances the injected clock instead of blocking, so backoff is
// simulated exactly and no test spends wall time.
func (r *recorder) sleep(ctx context.Context, d time.Duration) error {
	r.slept = append(r.slept, d)
	if r.onSleep != nil {
		return r.onSleep(ctx, d)
	}
	r.clk.Advance(d)
	return nil
}

// options returns the injection set every retry test uses: no real sleeping,
// deterministic jitter, and a manually advanced clock.
func (r *recorder) options(jitter float64) []RetryOption {
	return []RetryOption{
		WithRetrySleep(r.sleep),
		WithRetryRand(func() float64 { return jitter }),
		WithRetryClock(r.clk.Now),
		WithRetryHook(func(_ int, delay time.Duration, _ error) { r.retries = append(r.retries, delay) }),
	}
}

var (
	retryable    = errs.New(errs.CodeUpstreamTimeout, "the source did not answer in time")
	notRetryable = errs.New(errs.CodeUpstreamInvalidPayload, "the source returned something unusable")
)

// countingFn returns a function that always fails with err and counts calls.
func countingFn(err error, calls *int) func(context.Context) error {
	return func(context.Context) error {
		*calls++
		return err
	}
}

// ---------------------------------------------------------------------------
// Attempt counting
// ---------------------------------------------------------------------------

// TestRetrier_AttemptCounts verifies REQ-RES-002 and REQ-RES-003: retries are
// driven by the error taxonomy, never by "an error occurred", and they are
// bounded by configuration.
func TestRetrier_AttemptCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cfg         config.RetryConfig
		idempotent  bool
		err         error
		wantCalls   int
		wantRetries int
	}{
		{
			name:       "non-retryable error is attempted exactly once",
			cfg:        retryCfg(),
			idempotent: true,
			err:        notRetryable,
			wantCalls:  1,
		},
		{
			name:       "a client error is never retried",
			cfg:        retryCfg(),
			idempotent: true,
			err:        errs.New(errs.CodeInvalidRequest, "malformed identifier"),
			wantCalls:  1,
		},
		{
			name:       "a not-found is never retried",
			cfg:        retryCfg(),
			idempotent: true,
			err:        errs.ErrNotFound,
			wantCalls:  1,
		},
		{
			name:        "a retryable error is attempted up to max_attempts",
			cfg:         retryCfg(),
			idempotent:  true,
			err:         retryable,
			wantCalls:   3,
			wantRetries: 2,
		},
		{
			name:        "max_attempts of two",
			cfg:         withAttempts(retryCfg(), 2),
			idempotent:  true,
			err:         retryable,
			wantCalls:   2,
			wantRetries: 1,
		},
		{
			name:       "max_attempts of one means no retry at all",
			cfg:        withAttempts(retryCfg(), 1),
			idempotent: true,
			err:        retryable,
			wantCalls:  1,
		},
		{
			name:       "a non-idempotent call is never retried",
			cfg:        retryCfg(),
			idempotent: false,
			err:        retryable,
			wantCalls:  1,
		},
		{
			name:       "retry disabled means one attempt",
			cfg:        withEnabled(retryCfg(), false),
			idempotent: true,
			err:        retryable,
			wantCalls:  1,
		},
		{
			name:       "an explicitly non-retryable upstream error is not retried",
			cfg:        retryCfg(),
			idempotent: true,
			err:        errs.New(errs.CodeUpstreamTimeout, "timed out").NotRetryable(),
			wantCalls:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder()
			r := NewRetrier(tc.cfg, rec.options(1.0)...)

			calls := 0
			err := r.Do(context.Background(), tc.idempotent, countingFn(tc.err, &calls))

			testutil.True(t, errors.Is(err, tc.err), "the caller sees the source's own error")
			testutil.Equal(t, calls, tc.wantCalls, "attempts")
			testutil.Equal(t, len(rec.slept), tc.wantRetries, "backoff sleeps")
			testutil.Equal(t, len(rec.retries), tc.wantRetries, "retry hook invocations")
		})
	}
}

// TestRetrier_SucceedsWithoutRetrying verifies REQ-RES-002: a call that works
// costs nothing, and a call that recovers stops retrying at once.
func TestRetrier_SucceedsWithoutRetrying(t *testing.T) {
	t.Parallel()

	t.Run("first attempt succeeds", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		calls := 0
		err := r.Do(context.Background(), true, func(context.Context) error {
			calls++
			return nil
		})
		testutil.NoError(t, err, "a successful call")
		testutil.Equal(t, calls, 1, "no extra attempts")
		testutil.Equal(t, len(rec.slept), 0, "no backoff was paid")
	})

	t.Run("second attempt succeeds", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		calls := 0
		err := r.Do(context.Background(), true, func(context.Context) error {
			calls++
			if calls == 1 {
				return retryable
			}
			return nil
		})
		testutil.NoError(t, err, "the retry recovered")
		testutil.Equal(t, calls, 2, "exactly one retry")
		testutil.Equal(t, len(rec.slept), 1, "one backoff")
	})
}

// TestRetrier_ReturnsTheLastRealError verifies REQ-RES-002: the caller is shown
// the last attempt's actual error, never a synthesised "retries exhausted",
// because the real cause is what an operator needs.
func TestRetrier_ReturnsTheLastRealError(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	r := NewRetrier(retryCfg(), rec.options(1.0)...)

	attemptErrs := []error{
		errs.Wrap(errs.CodeUpstreamTimeout, "attempt one", errors.New("dial timeout")),
		errs.Wrap(errs.CodeUpstreamTimeout, "attempt two", errors.New("read timeout")),
		errs.Wrap(errs.CodeUpstreamUnavailable, "attempt three", errors.New("connection reset")),
	}

	calls := 0
	err := r.Do(context.Background(), true, func(context.Context) error {
		e := attemptErrs[calls]
		calls++
		return e
	})

	testutil.Equal(t, calls, 3, "every attempt was made")
	testutil.True(t, errors.Is(err, attemptErrs[2]), "the returned error is the last attempt's own error")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "and carries its code, not the first attempt's")
	e, ok := errs.As(err)
	testutil.True(t, ok, "the error is a taxonomy error")
	testutil.Equal(t, e.Message, "attempt three", "the message is the last attempt's")
}

// ---------------------------------------------------------------------------
// Backoff and jitter
// ---------------------------------------------------------------------------

// TestRetrier_BackoffGrowsAndIsCapped verifies REQ-RES-002: backoff is
// exponential and capped at max_backoff, so a long retry chain cannot grow an
// unbounded delay.
func TestRetrier_BackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	cfg := retryCfg()
	cfg.BaseBackoff = config.Duration(20 * time.Millisecond)
	cfg.MaxBackoff = config.Duration(100 * time.Millisecond)
	cfg.JitterFraction = 1.0

	rec := newRecorder()
	// rand() == 1 makes full jitter select the top of its range, which is the
	// undiluted exponential value.
	r := NewRetrier(cfg, rec.options(1.0)...)

	want := []time.Duration{
		20 * time.Millisecond,  // 20 * 2^0
		40 * time.Millisecond,  // 20 * 2^1
		80 * time.Millisecond,  // 20 * 2^2
		100 * time.Millisecond, // 160 capped at 100
		100 * time.Millisecond, // 320 capped at 100
		100 * time.Millisecond, // 640 capped at 100
	}
	for i, w := range want {
		testutil.Equal(t, r.backoff(i+1), w, "backoff for attempt %d", i+1)
	}

	t.Run("no base backoff means no delay", func(t *testing.T) {
		t.Parallel()
		noBase := cfg
		noBase.BaseBackoff = 0
		nr := NewRetrier(noBase, newRecorder().options(1.0)...)
		testutil.Equal(t, nr.backoff(1), time.Duration(0), "a zero base backoff yields no delay")
		testutil.Equal(t, nr.backoff(5), time.Duration(0), "and never grows")
	})

	t.Run("no cap means unbounded growth", func(t *testing.T) {
		t.Parallel()
		noCap := cfg
		noCap.MaxBackoff = 0
		nr := NewRetrier(noCap, newRecorder().options(1.0)...)
		testutil.Equal(t, nr.backoff(4), 160*time.Millisecond, "without a cap the exponential continues")
	})
}

// TestRetrier_Jitter verifies REQ-RES-002: jitter is mandatory, full jitter
// spreads a delay across [0, exp], and a partial fraction narrows the range to
// [exp*(1-f), exp]. Synchronised retries are what knock a recovering source
// over, so the spread is the point.
func TestRetrier_Jitter(t *testing.T) {
	t.Parallel()

	cfg := retryCfg()
	cfg.BaseBackoff = config.Duration(100 * time.Millisecond)
	cfg.MaxBackoff = config.Duration(time.Second)

	const exp = 100 * time.Millisecond // attempt 1

	t.Run("full jitter spans zero to the exponential value", func(t *testing.T) {
		t.Parallel()
		full := cfg
		full.JitterFraction = 1.0

		for _, draw := range []float64{0, 0.001, 0.25, 0.5, 0.75, 0.999, 1} {
			r := NewRetrier(full, WithRetryRand(func() float64 { return draw }))
			got := r.backoff(1)
			testutil.True(t, got >= 0 && got <= exp,
				"full jitter with draw %v must land in [0, %s], got %s", draw, exp, got)
			testutil.Equal(t, got, time.Duration(draw*float64(exp)), "full jitter is uniform over [0, exp]")
		}
	})

	t.Run("half jitter spans half the exponential value to all of it", func(t *testing.T) {
		t.Parallel()
		half := cfg
		half.JitterFraction = 0.5

		for _, draw := range []float64{0, 0.25, 0.5, 0.75, 1} {
			r := NewRetrier(half, WithRetryRand(func() float64 { return draw }))
			got := r.backoff(1)
			testutil.True(t, got >= exp/2 && got <= exp,
				"half jitter with draw %v must land in [%s, %s], got %s", draw, exp/2, exp, got)
		}

		lo := NewRetrier(half, WithRetryRand(func() float64 { return 0 }))
		hi := NewRetrier(half, WithRetryRand(func() float64 { return 1 }))
		testutil.Equal(t, lo.backoff(1), exp/2, "the bottom of the half-jitter range")
		testutil.Equal(t, hi.backoff(1), exp, "the top of the half-jitter range")
	})

	t.Run("zero jitter yields the bare exponential value", func(t *testing.T) {
		t.Parallel()
		none := cfg
		none.JitterFraction = 0
		r := NewRetrier(none, WithRetryRand(func() float64 {
			t.Fatal("the jitter source must not be consulted when jitter is off")
			return 0
		}))
		testutil.Equal(t, r.backoff(1), exp, "no jitter means the exponential value exactly")
	})

	t.Run("a fraction above one is clamped to full jitter", func(t *testing.T) {
		t.Parallel()
		over := cfg
		over.JitterFraction = 4
		r := NewRetrier(over, WithRetryRand(func() float64 { return 0 }))
		testutil.Equal(t, r.backoff(1), time.Duration(0),
			"a fraction above 1 behaves as full jitter, whose floor is zero")
	})

	t.Run("the jitter source is consulted once per backoff", func(t *testing.T) {
		t.Parallel()
		draws := 0
		full := cfg
		full.JitterFraction = 1.0
		r := NewRetrier(full, WithRetryRand(func() float64 { draws++; return 0.5 }))
		_ = r.backoff(1)
		_ = r.backoff(2)
		testutil.Equal(t, draws, 2, "one draw per computed backoff")
	})
}

// ---------------------------------------------------------------------------
// Budget and deadlines
// ---------------------------------------------------------------------------

// TestRetrier_BudgetStopsFurtherAttempts verifies REQ-RES-002: the retry budget
// caps total retry time as a fraction of the call deadline, so retries can
// never consume the whole request budget.
func TestRetrier_BudgetStopsFurtherAttempts(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	cfg := retryCfg()
	cfg.MaxAttempts = 5
	cfg.BaseBackoff = config.Duration(20 * time.Millisecond)
	cfg.MaxBackoff = config.Duration(time.Second)
	cfg.BudgetRatio = 0.01 // 100ms of a 10s deadline

	r := NewRetrier(cfg, rec.options(1.0)...)

	// The deadline is far enough out in real time that it cannot fire during
	// the test; only the retrier's arithmetic against it is under examination.
	ctx, cancel := context.WithDeadline(context.Background(), rec.clk.Now().Add(10*time.Second))
	defer cancel()

	calls := 0
	err := r.Do(ctx, true, countingFn(retryable, &calls))

	// 20ms then 40ms fit inside the 100ms budget; the next backoff would be
	// 80ms on top of 60ms already spent, which does not.
	testutil.Equal(t, calls, 3, "the budget stopped the chain before max_attempts")
	testutil.Equal(t, rec.slept, []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}, "backoffs paid")
	testutil.True(t, errors.Is(err, retryable), "the last real error is returned")

	t.Run("a generous budget allows every attempt", func(t *testing.T) {
		t.Parallel()
		rec2 := newRecorder()
		wide := cfg
		wide.BudgetRatio = 1.0
		r2 := NewRetrier(wide, rec2.options(1.0)...)

		ctx2, cancel2 := context.WithDeadline(context.Background(), rec2.clk.Now().Add(time.Hour))
		defer cancel2()

		calls2 := 0
		_ = r2.Do(ctx2, true, countingFn(retryable, &calls2))
		testutil.Equal(t, calls2, wide.MaxAttempts, "every configured attempt was made")
	})

	t.Run("no deadline means an unbounded budget", func(t *testing.T) {
		t.Parallel()
		rec3 := newRecorder()
		r3 := NewRetrier(cfg, rec3.options(1.0)...)
		calls3 := 0
		_ = r3.Do(context.Background(), true, countingFn(retryable, &calls3))
		testutil.Equal(t, calls3, cfg.MaxAttempts, "without a deadline the attempt count is the only bound")
	})
}

// TestRetrier_ContextDeadlineStopsFurtherAttempts verifies REQ-RES-002: the
// context always wins. A deadline that has already passed means no attempt at
// all, and one that fires mid-chain stops the chain.
func TestRetrier_ContextDeadlineStopsFurtherAttempts(t *testing.T) {
	t.Parallel()

	t.Run("an expired deadline prevents the first attempt", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		calls := 0
		err := r.Do(ctx, true, countingFn(retryable, &calls))
		testutil.Equal(t, calls, 0, "the source must not be called at all")
		testutil.ErrCode(t, err, errs.CodeUpstreamTimeout, "the failure is classified as a timeout")
		testutil.True(t, errors.Is(err, context.DeadlineExceeded), "the cause is the context's own error")
	})

	t.Run("a deadline that fires during backoff stops the chain", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		// The injected sleep reports that the context ended while waiting,
		// exactly as sleepCtx does when the deadline fires during backoff.
		rec.onSleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		calls := 0
		err := r.Do(context.Background(), true, countingFn(retryable, &calls))
		testutil.Equal(t, calls, 1, "the chain stops as soon as the backoff is interrupted")
		testutil.True(t, errors.Is(err, retryable), "the source's error is returned, not the sleep's")
	})

	t.Run("a context cancelled between attempts stops the chain", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel during the backoff, then report the sleep as complete, so the
		// loop re-checks ctx.Err() at the top of the next attempt.
		rec.onSleep = func(context.Context, time.Duration) error {
			cancel()
			return nil
		}
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		calls := 0
		err := r.Do(ctx, true, countingFn(retryable, &calls))
		testutil.Equal(t, calls, 1, "no attempt is made once the caller has gone away")
		testutil.True(t, errors.Is(err, retryable), "the last real error is still what the caller sees")
	})
}

// TestRetrier_CancelledContextIsNotRetried verifies REQ-RES-003: a cancelled
// context means the caller has gone away, so retrying is pure waste.
func TestRetrier_CancelledContextIsNotRetried(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{name: "bare context.Canceled", err: context.Canceled},
		{name: "wrapped in the taxonomy", err: errs.Wrap(errs.CodeUpstreamUnavailable, "call cancelled", context.Canceled)},
		{name: "wrapped by fmt", err: fmt.Errorf("calling the source: %w", context.Canceled)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder()
			r := NewRetrier(retryCfg(), rec.options(1.0)...)

			calls := 0
			err := r.Do(context.Background(), true, countingFn(tc.err, &calls))
			testutil.Equal(t, calls, 1, "a cancellation is never retried")
			testutil.Equal(t, len(rec.slept), 0, "and no backoff is paid")
			testutil.True(t, errors.Is(err, context.Canceled), "the cancellation is reported as-is")
		})
	}
}

// TestRetrier_PerAttemptTimeout verifies REQ-RES-001: an individual attempt can
// be capped, so one slow attempt cannot swallow the whole retry budget.
func TestRetrier_PerAttemptTimeout(t *testing.T) {
	t.Parallel()

	t.Run("no per-attempt timeout leaves the context alone", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		r := NewRetrier(retryCfg(), rec.options(1.0)...)

		testutil.NoError(t, r.Do(context.Background(), true, func(ctx context.Context) error {
			_, ok := ctx.Deadline()
			testutil.False(t, ok, "the attempt context must carry no deadline of its own")
			return nil
		}), "Do")
	})

	t.Run("a per-attempt timeout bounds each attempt", func(t *testing.T) {
		t.Parallel()
		cfg := retryCfg()
		cfg.PerAttemptTimeout = config.Duration(30 * time.Millisecond)
		rec := newRecorder()
		r := NewRetrier(cfg, rec.options(1.0)...)

		seen := 0
		testutil.NoError(t, r.Do(context.Background(), true, func(ctx context.Context) error {
			seen++
			dl, ok := ctx.Deadline()
			testutil.True(t, ok, "the attempt context carries a deadline")
			testutil.True(t, time.Until(dl) <= cfg.PerAttemptTimeout.D(),
				"the deadline is no further out than the configured per-attempt timeout")
			return nil
		}), "Do")
		testutil.Equal(t, seen, 1, "one attempt")
	})
}

// TestSleepCtx verifies the real sleep helper: a non-positive delay is free, a
// cancelled context returns immediately, and a short delay elapses.
func TestSleepCtx(t *testing.T) {
	t.Parallel()

	testutil.NoError(t, sleepCtx(context.Background(), 0), "a zero delay does not sleep")
	testutil.NoError(t, sleepCtx(context.Background(), -time.Second), "a negative delay does not sleep")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sleepCtx(ctx, time.Hour)
	testutil.True(t, errors.Is(err, context.Canceled), "a cancelled context ends the sleep at once")

	testutil.NoError(t, sleepCtx(context.Background(), time.Millisecond), "a short delay completes")
}

func withAttempts(cfg config.RetryConfig, n int) config.RetryConfig {
	cfg.MaxAttempts = n
	return cfg
}

func withEnabled(cfg config.RetryConfig, on bool) config.RetryConfig {
	cfg.Enabled = on
	return cfg
}
