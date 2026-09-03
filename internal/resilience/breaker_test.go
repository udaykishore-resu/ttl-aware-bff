package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/config"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// breakerEpoch is a fixed instant every breaker test starts from, so that no
// test depends on the wall clock.
var breakerEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// breakerCfg is the shared starting point; each test tightens what it cares
// about. The numbers are small so a test can exercise a threshold in a handful
// of calls.
func breakerCfg() config.BreakerConfig {
	return config.BreakerConfig{
		Enabled:           true,
		FailureThreshold:  0.5,
		MinimumRequests:   4,
		Window:            config.Duration(10 * time.Second),
		OpenTimeout:       config.Duration(5 * time.Second),
		HalfOpenMaxCalls:  2,
		HalfOpenSuccesses: 2,
	}
}

func newTestBreaker(t *testing.T, cfg config.BreakerConfig) (*Breaker, *testutil.Clock, *[]string) {
	t.Helper()
	clk := testutil.NewClock(breakerEpoch)
	transitions := &[]string{}
	b := NewBreaker("ods", cfg,
		WithBreakerClock(clk.Now),
		WithTransitionHook(func(_ string, from, to State) {
			*transitions = append(*transitions, from.String()+"->"+to.String())
		}),
	)
	return b, clk, transitions
}

var upstreamDown = errs.New(errs.CodeUpstreamUnavailable, "source refused the connection")

// TestState_String verifies that breaker states render the tokens used as
// metric attributes and log values (REQ-RES-005).
func TestState_String(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, StateClosed.String(), "closed", "closed")
	testutil.Equal(t, StateOpen.String(), "open", "open")
	testutil.Equal(t, StateHalfOpen.String(), "half-open", "half-open")
	testutil.Equal(t, State(99).String(), "unknown", "an undefined state renders as unknown")
}

// TestBreaker_Disabled verifies REQ-RES-005: a disabled breaker is a
// pass-through, so no call site needs a nil or enabled check.
func TestBreaker_Disabled(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.Enabled = false
	b, _, transitions := newTestBreaker(t, cfg)

	for i := 0; i < 50; i++ {
		testutil.NoError(t, b.Allow(), "a disabled breaker always allows")
		b.Record(upstreamDown)
	}
	testutil.Equal(t, b.State(), StateClosed, "a disabled breaker never leaves the closed state")
	testutil.Equal(t, len(*transitions), 0, "a disabled breaker never transitions")

	success, failure, rejected := b.Counts()
	testutil.Equal(t, success+failure+rejected, 0, "a disabled breaker records nothing")

	testutil.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }), "Do passes through")
	testutil.Equal(t, b.Name(), "ods", "the breaker keeps its identity for metrics")
}

// TestBreaker_StaysClosedBelowMinimumRequests verifies REQ-RES-005: the sample
// floor gates the failure ratio, so the first failure after a quiet period
// cannot trip the breaker at a nominal 100% failure rate.
func TestBreaker_StaysClosedBelowMinimumRequests(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.MinimumRequests = 6
	cfg.FailureThreshold = 0.5
	b, _, _ := newTestBreaker(t, cfg)

	for i := 1; i < cfg.MinimumRequests; i++ {
		b.Record(upstreamDown)
		testutil.Equal(t, b.State(), StateClosed,
			"after %d consecutive failures (minimum is %d) the breaker must stay closed", i, cfg.MinimumRequests)
		testutil.NoError(t, b.Allow(), "a closed breaker admits calls")
	}

	// The sample floor is now met and the ratio is 100%.
	b.Record(upstreamDown)
	testutil.Equal(t, b.State(), StateOpen, "the breaker trips once the sample floor is met")
}

// TestBreaker_TripsAtConfiguredRatio verifies REQ-RES-005: the breaker opens at
// the configured failure ratio and not before.
func TestBreaker_TripsAtConfiguredRatio(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		threshold float64
		successes int
		failures  int
		wantOpen  bool
	}{
		{name: "below threshold", threshold: 0.6, successes: 5, failures: 5, wantOpen: false},
		{name: "at threshold", threshold: 0.5, successes: 5, failures: 5, wantOpen: true},
		{name: "above threshold", threshold: 0.5, successes: 3, failures: 7, wantOpen: true},
		{name: "strict threshold not met", threshold: 1.0, successes: 1, failures: 9, wantOpen: false},
		{name: "strict threshold met", threshold: 1.0, successes: 0, failures: 10, wantOpen: true},
		{name: "lenient threshold", threshold: 0.2, successes: 8, failures: 2, wantOpen: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := breakerCfg()
			cfg.MinimumRequests = 10
			cfg.FailureThreshold = tc.threshold
			b, _, _ := newTestBreaker(t, cfg)

			// Successes first, so the sample floor is reached on the last
			// failure and the ratio decides the outcome.
			for i := 0; i < tc.successes; i++ {
				b.Record(nil)
			}
			for i := 0; i < tc.failures; i++ {
				b.Record(upstreamDown)
			}

			want := StateClosed
			if tc.wantOpen {
				want = StateOpen
			}
			testutil.Equal(t, b.State(), want,
				"%d successes and %d failures at threshold %v", tc.successes, tc.failures, tc.threshold)
		})
	}
}

// TestBreaker_OpenRejectsImmediately verifies REQ-RES-006: an open breaker
// fails fast with a classified error and never touches the source.
func TestBreaker_OpenRejectsImmediately(t *testing.T) {
	t.Parallel()

	b, _, transitions := newTestBreaker(t, breakerCfg())
	tripBreaker(t, b)
	testutil.Equal(t, *transitions, []string{"closed->open"}, "one transition, recorded for metrics")

	err := b.Allow()
	testutil.Error(t, err, "an open breaker refuses calls")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "the refusal is already classified")
	e, ok := errs.As(err)
	testutil.True(t, ok, "the error carries the taxonomy type")
	testutil.Equal(t, e.Source, "ods", "the refusal names the source")
	testutil.Equal(t, e.Op, "breaker.allow", "the refusal names the operation")
	testutil.True(t, e.Retryable, "an unavailable source may be retried later")

	called := false
	doErr := b.Do(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	testutil.ErrCode(t, doErr, errs.CodeUpstreamUnavailable, "Do refuses while open")
	testutil.False(t, called, "an open breaker must not invoke the source")

	_, _, rejected := b.Counts()
	testutil.Equal(t, rejected, 2, "rejections are counted for the admin surface")
}

// TestBreaker_HalfOpensAfterOpenTimeout verifies REQ-RES-005: the breaker
// probes again only once the open timeout has elapsed, measured on the injected
// clock so the test never sleeps.
func TestBreaker_HalfOpensAfterOpenTimeout(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	b, clk, transitions := newTestBreaker(t, cfg)
	tripBreaker(t, b)

	clk.Advance(cfg.OpenTimeout.D() - time.Nanosecond)
	testutil.Equal(t, b.State(), StateOpen, "one nanosecond early, the breaker is still open")
	testutil.ErrCode(t, b.Allow(), errs.CodeUpstreamUnavailable, "and still refuses calls")

	clk.Advance(time.Nanosecond)
	testutil.Equal(t, b.State(), StateHalfOpen, "at the open timeout the breaker begins probing")
	// The open->half-open step is reported like any other, so a dashboard can
	// see that a breaker has started probing rather than only that it opened
	// and later closed.
	testutil.Equal(t, *transitions, []string{"closed->open", "open->half-open"}, "reported transitions")

	testutil.NoError(t, b.Allow(), "a half-open breaker admits a probe")
}

// TestBreaker_HalfOpenBoundsConcurrentProbes verifies REQ-RES-005: half-open
// admits at most half_open_max_calls probes, so a recovering source is not hit
// with full traffic.
func TestBreaker_HalfOpenBoundsConcurrentProbes(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.HalfOpenMaxCalls = 3
	cfg.HalfOpenSuccesses = 10 // high, so the probes below cannot close it
	b, clk, _ := newTestBreaker(t, cfg)
	tripBreaker(t, b)
	clk.Advance(cfg.OpenTimeout.D())

	for i := 1; i <= cfg.HalfOpenMaxCalls; i++ {
		testutil.NoError(t, b.Allow(), "probe %d of %d must be admitted", i, cfg.HalfOpenMaxCalls)
	}

	err := b.Allow()
	testutil.Error(t, err, "the probe budget is exhausted")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "the refusal is classified")
	e, _ := errs.As(err)
	testutil.Equal(t, e.Op, "breaker.half_open_saturated", "the refusal explains itself")
	testutil.Equal(t, b.State(), StateHalfOpen, "the breaker is still probing")

	// Completing a probe frees its slot for the next one.
	b.Record(nil)
	testutil.NoError(t, b.Allow(), "a completed probe releases its slot")
}

// TestBreaker_ClosesAfterConsecutiveHalfOpenSuccesses verifies REQ-RES-005: one
// lucky call does not re-admit full traffic; the configured number of
// successful probes does.
func TestBreaker_ClosesAfterConsecutiveHalfOpenSuccesses(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.HalfOpenSuccesses = 3
	cfg.HalfOpenMaxCalls = 1
	b, clk, transitions := newTestBreaker(t, cfg)
	tripBreaker(t, b)
	clk.Advance(cfg.OpenTimeout.D())

	for i := 1; i < cfg.HalfOpenSuccesses; i++ {
		testutil.NoError(t, b.Allow(), "probe %d admitted", i)
		b.Record(nil)
		testutil.Equal(t, b.State(), StateHalfOpen,
			"after %d of %d successful probes the breaker is still half-open", i, cfg.HalfOpenSuccesses)
	}

	testutil.NoError(t, b.Allow(), "the final probe is admitted")
	b.Record(nil)
	testutil.Equal(t, b.State(), StateClosed, "the breaker closes on the last required success")
	// The open->half-open step is reported by the hook (see
	// TestBreaker_HalfOpensAfterOpenTimeout).
	testutil.Equal(t, *transitions, []string{"closed->open", "open->half-open", "half-open->closed"}, "reported transitions")
}

// TestBreaker_ReopensOnSingleHalfOpenFailure verifies REQ-RES-005: a failure
// while probing means the source is still unwell, so the breaker re-opens
// immediately rather than waiting for a ratio.
func TestBreaker_ReopensOnSingleHalfOpenFailure(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.HalfOpenSuccesses = 3
	b, clk, transitions := newTestBreaker(t, cfg)
	tripBreaker(t, b)
	clk.Advance(cfg.OpenTimeout.D())

	// Two successful probes, then one failure: the successes count for nothing.
	testutil.NoError(t, b.Allow(), "first probe")
	b.Record(nil)
	testutil.NoError(t, b.Allow(), "second probe")
	b.Record(upstreamDown)

	testutil.Equal(t, b.State(), StateOpen, "a single probe failure re-opens the breaker")
	testutil.Equal(t, *transitions,
		[]string{"closed->open", "open->half-open", "half-open->open"}, "reported transitions")
	testutil.ErrCode(t, b.Allow(), errs.CodeUpstreamUnavailable, "and calls are refused again")

	// The open timer restarts from the re-open, not from the original trip.
	clk.Advance(cfg.OpenTimeout.D() - time.Nanosecond)
	testutil.Equal(t, b.State(), StateOpen, "the open timeout is measured from the re-open")
	clk.Advance(time.Nanosecond)
	testutil.Equal(t, b.State(), StateHalfOpen, "and then probes again")
}

// TestBreaker_ClosingClearsTheWindow verifies REQ-RES-005: the failures that
// caused the outage are forgotten when the breaker closes, so the first new
// failure cannot instantly re-trip it.
func TestBreaker_ClosingClearsTheWindow(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.MinimumRequests = 4
	cfg.FailureThreshold = 0.5
	cfg.HalfOpenSuccesses = 2
	b, clk, _ := newTestBreaker(t, cfg)

	tripBreaker(t, b)
	_, failure, _ := b.Counts()
	testutil.Equal(t, failure, 4, "the outage's failures are in the window")

	clk.Advance(cfg.OpenTimeout.D())
	for i := 0; i < cfg.HalfOpenSuccesses; i++ {
		testutil.NoError(t, b.Allow(), "probe %d", i+1)
		b.Record(nil)
	}
	testutil.Equal(t, b.State(), StateClosed, "the breaker closed")

	success, failure, rejected := b.Counts()
	testutil.Equal(t, success, 0, "closing clears the window's successes")
	testutil.Equal(t, failure, 0, "closing clears the window's failures")
	testutil.Equal(t, rejected, 0, "closing clears the window's rejections")

	// One new failure must not re-trip: the sample floor has to be met again
	// from scratch.
	b.Record(upstreamDown)
	testutil.Equal(t, b.State(), StateClosed,
		"a single post-recovery failure must not re-trip the breaker")

	for i := 1; i < cfg.MinimumRequests; i++ {
		b.Record(upstreamDown)
	}
	testutil.Equal(t, b.State(), StateOpen, "a genuine new outage still trips it")
}

// TestBreaker_WindowAgesOut verifies REQ-RES-005: the rolling window is a ring
// of buckets, so failures age out gradually rather than being cleared wholesale
// on a tick boundary.
func TestBreaker_WindowAgesOut(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.MinimumRequests = 4
	b, clk, _ := newTestBreaker(t, cfg)

	// Three failures, then wait out the whole window.
	for i := 0; i < 3; i++ {
		b.Record(upstreamDown)
	}
	_, failure, _ := b.Counts()
	testutil.Equal(t, failure, 3, "the failures are in the window")

	clk.Advance(cfg.Window.D() + time.Second)
	_, failure, _ = b.Counts()
	testutil.Equal(t, failure, 0, "failures older than the window are not counted")

	// And they cannot contribute to a trip either.
	for i := 0; i < 3; i++ {
		b.Record(upstreamDown)
	}
	testutil.Equal(t, b.State(), StateClosed,
		"three fresh failures plus three aged-out ones must not meet a floor of four")
}

// TestBreaker_Do verifies REQ-RES-005: Do reports the outcome to the breaker and
// returns the source's error untouched.
func TestBreaker_Do(t *testing.T) {
	t.Parallel()

	b, _, _ := newTestBreaker(t, breakerCfg())

	testutil.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }), "a successful call")
	success, _, _ := b.Counts()
	testutil.Equal(t, success, 1, "the success is recorded")

	sentinel := errors.New("dial tcp: connection refused")
	wrapped := errs.Wrap(errs.CodeUpstreamUnavailable, "source refused the connection", sentinel)
	got := b.Do(context.Background(), func(context.Context) error { return wrapped })
	testutil.True(t, errors.Is(got, sentinel), "Do returns the source's own error, wrapping and all")

	_, failure, _ := b.Counts()
	testutil.Equal(t, failure, 1, "the failure is recorded")

	// The call's context reaches the function unchanged.
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")
	testutil.NoError(t, b.Do(ctx, func(inner context.Context) error {
		testutil.Equal(t, inner.Value(ctxKey{}), any("v"), "the caller's context is passed through")
		return nil
	}), "context passthrough")
}

// TestBreaker_ClientFaultsDoNotOpenTheCircuit verifies REQ-RES-005 and
// REQ-RES-003: an error that is the caller's fault says nothing about the
// source's health, so it must never contribute to the failure ratio. A 404 loop
// from a UI polling a deleted resource must not take the source out of service.
func TestBreaker_ClientFaultsDoNotOpenTheCircuit(t *testing.T) {
	t.Parallel()

	clientFaults := []struct {
		name string
		err  error
	}{
		{"not found", errs.New(errs.CodeNotFound, "resource not found")},
		{"invalid request", errs.New(errs.CodeInvalidRequest, "malformed identifier")},
		{"forbidden", errs.New(errs.CodeForbidden, "not permitted")},
		{"unauthenticated", errs.New(errs.CodeUnauthenticated, "no credentials")},
		{"tenant mismatch", errs.New(errs.CodeTenantMismatch, "tenant does not match")},
	}

	for _, tc := range clientFaults {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := breakerCfg()
			cfg.MinimumRequests = 1
			cfg.FailureThreshold = 0.01 // any recorded failure at all would trip
			b, _, transitions := newTestBreaker(t, cfg)

			for i := 0; i < 20; i++ {
				err := b.Do(context.Background(), func(context.Context) error { return tc.err })
				testutil.True(t, errors.Is(err, tc.err), "Do returns the client error unchanged")
			}

			testutil.Equal(t, b.State(), StateClosed,
				"a client-caused error must never open the circuit")
			testutil.Equal(t, len(*transitions), 0, "no state change at all")

			success, failure, _ := b.Counts()
			testutil.Equal(t, failure, 0, "client faults are not counted as failures")
			testutil.Equal(t, success, 0,
				"nor as successes: a client fault is evidence about the caller, not about the "+
					"source, and counting it either way would let a source that answers nothing "+
					"but 404s while it is down look healthy")
		})
	}

	t.Run("a client fault in half-open returns its probe slot", func(t *testing.T) {
		t.Parallel()
		// Half-open admission and outcome recording are separate steps: Allow
		// takes a slot, Record or Abstain gives it back. If an abstaining call
		// kept its slot, half_open_max_calls client faults would hold every
		// probe slot until the next open timeout and stall recovery on a source
		// that was never the problem.
		cfg := breakerCfg()
		cfg.HalfOpenMaxCalls = 2
		cfg.HalfOpenSuccesses = 5 // high enough that probing cannot close it here
		b, clk, _ := newTestBreaker(t, cfg)
		tripBreaker(t, b)
		clk.Advance(cfg.OpenTimeout.D())
		testutil.Equal(t, b.State(), StateHalfOpen, "the breaker is probing")

		clientFault := errs.New(errs.CodeNotFound, "no such resource")
		for i := 0; i < cfg.HalfOpenMaxCalls*3; i++ {
			err := b.Do(context.Background(), func(context.Context) error { return clientFault })
			testutil.True(t, errors.Is(err, clientFault), "the client error is returned unchanged")
		}

		testutil.NoError(t, b.Allow(),
			"probe slots are still available: abstaining calls returned theirs")
		testutil.Equal(t, b.State(), StateHalfOpen, "and the breaker is still probing")
	})

	t.Run("a genuine source fault still trips", func(t *testing.T) {
		t.Parallel()
		cfg := breakerCfg()
		cfg.MinimumRequests = 1
		cfg.FailureThreshold = 0.01
		b, _, _ := newTestBreaker(t, cfg)

		_ = b.Do(context.Background(), func(context.Context) error { return upstreamDown })
		testutil.Equal(t, b.State(), StateOpen, "an upstream failure does open the circuit")
	})

	t.Run("an unclassified error is treated as a source fault", func(t *testing.T) {
		t.Parallel()
		cfg := breakerCfg()
		cfg.MinimumRequests = 1
		cfg.FailureThreshold = 0.01
		b, _, _ := newTestBreaker(t, cfg)

		_ = b.Do(context.Background(), func(context.Context) error { return errors.New("boom") })
		testutil.Equal(t, b.State(), StateOpen,
			"an error with no taxonomy code is assumed to be the source's fault")
	})
}

// TestBreaker_RecordIgnoresClientFaultsOnlyThroughDo documents where the
// client-fault filter lives: Record itself is unfiltered, and it is Do (and the
// adapters) that decide what counts (REQ-RES-005).
func TestBreaker_RecordIgnoresClientFaultsOnlyThroughDo(t *testing.T) {
	t.Parallel()

	cfg := breakerCfg()
	cfg.MinimumRequests = 1
	cfg.FailureThreshold = 0.01
	b, _, _ := newTestBreaker(t, cfg)

	b.Record(errs.New(errs.CodeNotFound, "resource not found"))
	testutil.Equal(t, b.State(), StateOpen,
		"Record is the raw primitive; filtering client faults is Do's job")
}

// tripBreaker drives a freshly built breaker to the open state using exactly
// the configured minimum number of failures.
func tripBreaker(t *testing.T, b *Breaker) {
	t.Helper()
	for i := 0; i < b.cfg.MinimumRequests; i++ {
		b.Record(upstreamDown)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("expected the breaker to be open after %d failures, got %s", b.cfg.MinimumRequests, got)
	}
}
