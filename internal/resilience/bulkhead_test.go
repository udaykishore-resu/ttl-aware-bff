package resilience

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

func bulkheadCfg(maxConcurrent, maxQueue int, acquire time.Duration) config.BulkheadConfig {
	return config.BulkheadConfig{
		Enabled:        true,
		MaxConcurrent:  maxConcurrent,
		MaxQueue:       maxQueue,
		AcquireTimeout: config.Duration(acquire),
	}
}

// waitFor spins until cond holds, yielding to the scheduler between checks. It
// is used only where a test genuinely has other goroutines in flight; it has a
// generous ceiling so it cannot be flaky under load, and it never sleeps for a
// fixed period.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// TestBulkhead_Disabled verifies REQ-RES-007: a disabled bulkhead is a
// pass-through, so a call site never needs to know whether one is configured.
func TestBulkhead_Disabled(t *testing.T) {
	t.Parallel()

	var changes int64
	b := NewBulkhead("eds", config.BulkheadConfig{Enabled: false, MaxConcurrent: 1},
		func(string, int64) { atomic.AddInt64(&changes, 1) })

	var releases []func()
	for i := 0; i < 100; i++ {
		release, err := b.Acquire(context.Background())
		testutil.NoError(t, err, "a disabled bulkhead admits every call")
		testutil.True(t, release != nil, "a release function is always returned")
		releases = append(releases, release)
	}
	testutil.False(t, b.Saturated(), "a disabled bulkhead is never saturated")

	inFlight, waiting, rejected, capacity := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "a disabled bulkhead reports no occupancy")
	testutil.Equal(t, waiting, int64(0), "no waiters")
	testutil.Equal(t, rejected, int64(0), "no rejections")
	testutil.Equal(t, capacity, 0, "no capacity to report")

	for _, r := range releases {
		r()
	}
	testutil.Equal(t, atomic.LoadInt64(&changes), int64(0), "no gauge deltas are emitted")

	testutil.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }), "Do passes through")
}

// TestBulkhead_AdmitsUpToMaxConcurrent verifies REQ-RES-007: the bulkhead bounds
// concurrency per source, and the gauge hook sees one delta per admission and
// one per release.
func TestBulkhead_AdmitsUpToMaxConcurrent(t *testing.T) {
	t.Parallel()

	var delta int64
	b := NewBulkhead("eds", bulkheadCfg(3, 0, 0), func(_ string, d int64) { atomic.AddInt64(&delta, d) })

	var releases []func()
	for i := 1; i <= 3; i++ {
		release, err := b.Acquire(context.Background())
		testutil.NoError(t, err, "slot %d of 3 must be admitted", i)
		releases = append(releases, release)

		inFlight, _, _, capacity := b.Stats()
		testutil.Equal(t, inFlight, int64(i), "in-flight count after %d admissions", i)
		testutil.Equal(t, capacity, 3, "reported capacity")
	}
	testutil.Equal(t, atomic.LoadInt64(&delta), int64(3), "the gauge saw three admissions")
	testutil.True(t, b.Saturated(), "every slot is held")

	// The queue has zero capacity, so the fourth caller is refused outright.
	release, err := b.Acquire(context.Background())
	testutil.Error(t, err, "the fourth caller must be refused")
	testutil.True(t, release == nil, "no release function is handed out on refusal")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "backpressure is reported as source unavailability")

	releases[0]()
	testutil.False(t, b.Saturated(), "releasing a slot un-saturates the bulkhead")
	release, err = b.Acquire(context.Background())
	testutil.NoError(t, err, "the freed slot is reusable")
	releases = append(releases[1:], release)

	for _, r := range releases {
		r()
	}
	inFlight, _, _, _ := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "every slot is returned")
	testutil.Equal(t, atomic.LoadInt64(&delta), int64(0), "admissions and releases balance")
}

// TestBulkhead_QueuesThenRejects verifies REQ-RES-007: waiters are bounded too.
// An unbounded queue in front of a saturated source only moves the failure
// later and makes it worse, so the queue has a fixed depth and anything beyond
// it is refused immediately.
func TestBulkhead_QueuesThenRejects(t *testing.T) {
	t.Parallel()

	const (
		maxConcurrent = 2
		maxQueue      = 3
	)
	b := NewBulkhead("eds", bulkheadCfg(maxConcurrent, maxQueue, 0), nil)

	// Occupy every slot.
	var held []func()
	for i := 0; i < maxConcurrent; i++ {
		release, err := b.Acquire(context.Background())
		testutil.NoError(t, err, "slot %d", i)
		held = append(held, release)
	}

	// Fill the queue with genuine waiters. They block until a slot frees.
	waiterCtx, cancelWaiters := context.WithCancel(context.Background())
	defer cancelWaiters()

	var wg sync.WaitGroup
	var admitted atomic.Int64
	for i := 0; i < maxQueue; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Release as soon as the slot arrives: holding it here would
			// deadlock the waiters behind this one.
			release, err := b.Acquire(waiterCtx)
			if err != nil {
				return
			}
			admitted.Add(1)
			release()
		}()
	}
	waitFor(t, func() bool {
		_, waiting, _, _ := b.Stats()
		return waiting == int64(maxQueue)
	}, "the queue to fill")

	// One more caller finds both the slots and the queue full.
	release, err := b.Acquire(context.Background())
	testutil.Error(t, err, "a caller beyond slots plus queue must be refused")
	testutil.True(t, release == nil, "no release function on refusal")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "the refusal is classified")
	e, ok := errs.As(err)
	testutil.True(t, ok, "the error carries the taxonomy type")
	testutil.Equal(t, e.Source, "eds", "the refusal names the source")
	testutil.Equal(t, e.Op, "bulkhead.queue_full", "the refusal explains that the queue, not a timeout, refused it")

	_, _, rejected, _ := b.Stats()
	testutil.Equal(t, rejected, int64(1), "the rejection is counted")

	// Freeing the held slots lets the queued waiters through.
	for _, r := range held {
		r()
	}
	wg.Wait()
	testutil.Equal(t, admitted.Load(), int64(maxQueue), "every queued waiter eventually ran")

	inFlight, waiting, _, _ := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "no slot is leaked")
	testutil.Equal(t, waiting, int64(0), "no waiter is leaked")
}

// TestBulkhead_AcquireTimeout verifies REQ-RES-007: a queued caller waits only
// as long as the configured acquire timeout, so backpressure reaches the caller
// while it can still act on it.
func TestBulkhead_AcquireTimeout(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", bulkheadCfg(1, 1, 5*time.Millisecond), nil)

	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "the only slot")
	defer release()

	start := time.Now()
	_, err = b.Acquire(context.Background())
	elapsed := time.Since(start)

	testutil.Error(t, err, "the queued caller must give up")
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "an acquire timeout is source unavailability")
	e, _ := errs.As(err)
	testutil.Equal(t, e.Op, "bulkhead.acquire_timeout", "the refusal explains itself")
	testutil.True(t, elapsed >= 5*time.Millisecond, "it waited for the configured timeout, took %s", elapsed)

	_, waiting, rejected, _ := b.Stats()
	testutil.Equal(t, waiting, int64(0), "the waiter slot is returned")
	testutil.Equal(t, rejected, int64(1), "the rejection is counted")
}

// TestBulkhead_ContextCancellation verifies REQ-RES-011: a caller that goes away
// while queued stops waiting, and its abandonment is reported as a timeout
// rather than as a bulkhead refusal.
func TestBulkhead_ContextCancellation(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", bulkheadCfg(1, 1, 0), nil)

	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "the only slot")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := b.Acquire(ctx)
	testutil.Error(t, err, "a cancelled caller must not wait")
	testutil.True(t, got == nil, "no release function")
	testutil.ErrCode(t, err, errs.CodeUpstreamTimeout, "cancellation while queued is a timeout")
	e, _ := errs.As(err)
	testutil.Equal(t, e.Op, "bulkhead.ctx_done", "the refusal explains itself")

	_, _, rejected, _ := b.Stats()
	testutil.Equal(t, rejected, int64(1), "the rejection is counted")
}

// TestBulkhead_ReleaseIsIdempotent verifies REQ-RES-007: the release function is
// safe to call more than once. A double release that decremented twice would
// hand out a slot that does not exist, which is exactly the leak the bulkhead
// exists to prevent.
func TestBulkhead_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	var delta int64
	b := NewBulkhead("eds", bulkheadCfg(2, 0, 0), func(_ string, d int64) { atomic.AddInt64(&delta, d) })

	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "acquire")
	inFlight, _, _, _ := b.Stats()
	testutil.Equal(t, inFlight, int64(1), "one slot held")
	testutil.Equal(t, atomic.LoadInt64(&delta), int64(1), "one gauge increment")

	release()
	inFlight, _, _, _ = b.Stats()
	testutil.Equal(t, inFlight, int64(0), "the slot is returned")
	testutil.Equal(t, atomic.LoadInt64(&delta), int64(0), "the gauge is balanced")

	release()
	release()
	release()
	inFlight, _, _, _ = b.Stats()
	testutil.Equal(t, inFlight, int64(0), "repeated releases must not drive the counter negative")
	testutil.Equal(t, atomic.LoadInt64(&delta), int64(0), "and must not emit further gauge deltas")

	// Both slots are still genuinely available.
	r1, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "first slot after the double release")
	r2, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "second slot after the double release")
	testutil.True(t, b.Saturated(), "and the bulkhead still knows it is full")
	r1()
	r2()
}

// TestBulkhead_ReleaseIsIdempotentUnderConcurrency verifies the same invariant
// when several goroutines race to release the same slot (REQ-RES-007).
func TestBulkhead_ReleaseIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", bulkheadCfg(4, 0, 0), nil)
	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "acquire")

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release()
		}()
	}
	wg.Wait()

	inFlight, _, _, _ := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "exactly one release took effect")

	// All four slots must still be obtainable.
	var held []func()
	for i := 0; i < 4; i++ {
		r, err := b.Acquire(context.Background())
		testutil.NoError(t, err, "slot %d is still available", i)
		held = append(held, r)
	}
	for _, r := range held {
		r()
	}
}

// TestBulkhead_Saturated verifies REQ-RES-007: the router consults Saturated as
// a health signal before choosing a source, so it must be exact at the boundary.
func TestBulkhead_Saturated(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", bulkheadCfg(2, 4, 0), nil)
	testutil.False(t, b.Saturated(), "an idle bulkhead is not saturated")

	r1, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "first slot")
	testutil.False(t, b.Saturated(), "one of two slots held is not saturation")

	r2, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "second slot")
	testutil.True(t, b.Saturated(), "both slots held is saturation")

	r1()
	testutil.False(t, b.Saturated(), "releasing one slot clears saturation")
	r2()
	testutil.False(t, b.Saturated(), "and releasing the other keeps it clear")
}

// TestBulkhead_Do verifies REQ-RES-007: Do holds a slot for the duration of the
// call, returns the call's error unchanged, and releases even when the call
// panics is not claimed -- but it does release on a normal error path.
func TestBulkhead_Do(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", bulkheadCfg(1, 0, 0), nil)

	testutil.NoError(t, b.Do(context.Background(), func(context.Context) error {
		inFlight, _, _, _ := b.Stats()
		testutil.Equal(t, inFlight, int64(1), "the slot is held for the duration of the call")
		return nil
	}), "Do")

	inFlight, _, _, _ := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "the slot is released afterwards")

	sentinel := errs.New(errs.CodeUpstreamTimeout, "source timed out")
	testutil.ErrCode(t, b.Do(context.Background(), func(context.Context) error { return sentinel }),
		errs.CodeUpstreamTimeout, "Do returns the call's own error")
	inFlight, _, _, _ = b.Stats()
	testutil.Equal(t, inFlight, int64(0), "the slot is released after a failure too")

	// A Do that cannot get a slot never runs the function.
	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "occupy the only slot")
	called := false
	err = b.Do(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "Do reports the refusal")
	testutil.False(t, called, "a refused Do must not touch the source")
	release()
}

// TestBulkhead_ConcurrentHammer verifies REQ-RES-007 under contention: the
// concurrency bound is never exceeded, counters balance, and nothing races.
// Meaningful under -race, which is how the suite runs.
func TestBulkhead_ConcurrentHammer(t *testing.T) {
	t.Parallel()

	const (
		maxConcurrent = 8
		goroutines    = 64
		iterations    = 50
	)
	b := NewBulkhead("eds", bulkheadCfg(maxConcurrent, 128, 50*time.Millisecond), nil)

	var (
		live     atomic.Int64
		peak     atomic.Int64
		admits   atomic.Int64
		refusals atomic.Int64
	)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				err := b.Do(context.Background(), func(context.Context) error {
					n := live.Add(1)
					for {
						p := peak.Load()
						if n <= p || peak.CompareAndSwap(p, n) {
							break
						}
					}
					runtime.Gosched()
					live.Add(-1)
					return nil
				})
				if err != nil {
					refusals.Add(1)
					continue
				}
				admits.Add(1)
			}
		}()
	}
	wg.Wait()

	testutil.True(t, peak.Load() <= maxConcurrent,
		"concurrency peaked at %d, above the bound of %d", peak.Load(), maxConcurrent)
	testutil.True(t, peak.Load() > 1, "the test did not actually exercise concurrency (peak %d)", peak.Load())
	testutil.Equal(t, live.Load(), int64(0), "no call is left in flight")
	testutil.Equal(t, admits.Load()+refusals.Load(), int64(goroutines*iterations), "every call was accounted for")

	inFlight, waiting, _, capacity := b.Stats()
	testutil.Equal(t, inFlight, int64(0), "every slot was returned")
	testutil.Equal(t, waiting, int64(0), "every waiter left the queue")
	testutil.Equal(t, capacity, maxConcurrent, "capacity is unchanged")
	testutil.False(t, b.Saturated(), "the bulkhead is idle again")
}

// TestBulkhead_DegenerateConfiguration verifies that a nonsensical bulkhead
// configuration is clamped rather than panicking (REQ-RES-007). Validation
// refuses these values at load time; this asserts the mechanism is safe anyway.
func TestBulkhead_DegenerateConfiguration(t *testing.T) {
	t.Parallel()

	b := NewBulkhead("eds", config.BulkheadConfig{Enabled: true, MaxConcurrent: 0, MaxQueue: -5}, nil)
	_, _, _, capacity := b.Stats()
	testutil.Equal(t, capacity, 1, "a max_concurrent below one is clamped to one slot")

	release, err := b.Acquire(context.Background())
	testutil.NoError(t, err, "the single slot is usable")
	_, err = b.Acquire(context.Background())
	testutil.ErrCode(t, err, errs.CodeUpstreamUnavailable, "a negative queue depth behaves as no queue")
	release()
}
