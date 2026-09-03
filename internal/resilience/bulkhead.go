package resilience

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// Bulkhead bounds the number of concurrent calls to one data source.
//
// This is the mechanism that stops the slow Execution source from taking the
// whole BFF down: if the EDS starts answering in ten seconds instead of one,
// its bulkhead saturates and further EDS calls are rejected fast, while
// Operational calls -- which hold a different bulkhead -- keep flowing. Without
// it, every in-flight request would sit blocked on the EDS and the BFF would
// run out of goroutines, connections and, eventually, memory (REQ-RES-005).
//
// Waiters are bounded too. An unbounded queue in front of a saturated resource
// just moves the failure later and makes it worse.
type Bulkhead struct {
	name      string
	enabled   bool
	sem       chan struct{}
	waitSlots chan struct{}
	acquire   time.Duration

	inFlight atomic.Int64
	waiting  atomic.Int64
	rejected atomic.Int64
	// onChange reports in-flight deltas for the gauge.
	onChange func(name string, delta int64)
}

// NewBulkhead builds a bulkhead. A disabled configuration yields a pass-through.
func NewBulkhead(name string, cfg config.BulkheadConfig, onChange func(string, int64)) *Bulkhead {
	b := &Bulkhead{
		name:     name,
		enabled:  cfg.Enabled,
		acquire:  cfg.AcquireTimeout.D(),
		onChange: onChange,
	}
	if !cfg.Enabled {
		return b
	}
	n := cfg.MaxConcurrent
	if n < 1 {
		n = 1
	}
	b.sem = make(chan struct{}, n)
	q := cfg.MaxQueue
	if q < 0 {
		q = 0
	}
	b.waitSlots = make(chan struct{}, q)
	return b
}

// Acquire takes a slot, blocking up to the configured acquire timeout.
// The returned release function must be called exactly once.
func (b *Bulkhead) Acquire(ctx context.Context) (release func(), err error) {
	if !b.enabled {
		return func() {}, nil
	}

	// Fast path: a free slot, no queueing.
	select {
	case b.sem <- struct{}{}:
		b.enter()
		return b.releaseFn(), nil
	default:
	}

	// Queue admission. A full queue is rejected immediately rather than
	// waiting, so backpressure reaches the caller while it can still act.
	select {
	case b.waitSlots <- struct{}{}:
	default:
		b.rejected.Add(1)
		return nil, errs.ErrBulkheadFull.WithSource(b.name).WithOp("bulkhead.queue_full")
	}
	defer func() { <-b.waitSlots }()

	b.waiting.Add(1)
	defer b.waiting.Add(-1)

	var timeout <-chan time.Time
	if b.acquire > 0 {
		t := time.NewTimer(b.acquire)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case b.sem <- struct{}{}:
		b.enter()
		return b.releaseFn(), nil
	case <-timeout:
		b.rejected.Add(1)
		return nil, errs.ErrBulkheadFull.WithSource(b.name).WithOp("bulkhead.acquire_timeout")
	case <-ctx.Done():
		b.rejected.Add(1)
		return nil, errs.Wrap(errs.CodeUpstreamTimeout, "cancelled while waiting for source capacity", ctx.Err()).
			WithSource(b.name).WithOp("bulkhead.ctx_done")
	}
}

// Do runs fn while holding a slot.
func (b *Bulkhead) Do(ctx context.Context, fn func(context.Context) error) error {
	release, err := b.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// Stats reports current occupancy, for metrics and the admin surface.
func (b *Bulkhead) Stats() (inFlight, waiting, rejected int64, capacity int) {
	if !b.enabled {
		return 0, 0, 0, 0
	}
	return b.inFlight.Load(), b.waiting.Load(), b.rejected.Load(), cap(b.sem)
}

// Saturated reports whether every slot is currently held. The router consults
// this as a health signal before choosing a source.
func (b *Bulkhead) Saturated() bool {
	if !b.enabled {
		return false
	}
	return b.inFlight.Load() >= int64(cap(b.sem))
}

func (b *Bulkhead) enter() {
	b.inFlight.Add(1)
	if b.onChange != nil {
		b.onChange(b.name, 1)
	}
}

func (b *Bulkhead) releaseFn() func() {
	var once atomic.Bool
	return func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		<-b.sem
		b.inFlight.Add(-1)
		if b.onChange != nil {
			b.onChange(b.name, -1)
		}
	}
}
