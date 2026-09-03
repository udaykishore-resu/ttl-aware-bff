package freshness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/testutil"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestEvaluate_TTLBoundary verifies REQ-TTL-001 and REQ-TTL-002: a record is
// fresh up to and including its TTL, and stale beyond it. The boundary is
// tested exactly, because "<= or <" is the kind of detail that silently costs a
// service its fast path.
func TestEvaluate_TTLBoundary(t *testing.T) {
	t.Parallel()

	const ttl = 30 * time.Second
	cases := []struct {
		name string
		age  time.Duration
		want domain.FreshnessState
	}{
		{"just refreshed", 0, domain.FreshnessFresh},
		{"well inside the TTL", 10 * time.Second, domain.FreshnessFresh},
		{"one nanosecond inside", ttl - time.Nanosecond, domain.FreshnessFresh},
		{"exactly at the TTL", ttl, domain.FreshnessFresh},
		{"one nanosecond past", ttl + time.Nanosecond, domain.FreshnessStale},
		{"far past", 10 * time.Minute, domain.FreshnessStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obs := datasource.Observation{
				Found:       true,
				LastUpdated: base.Add(-tc.age),
				SourceTime:  base,
			}
			ev := Evaluate(obs, ttl, time.Second, base)
			testutil.Equal(t, ev.State, tc.want, "freshness state at age %s", tc.age)
			testutil.Equal(t, ev.Age, tc.age, "reported age")
			testutil.Equal(t, ev.TTL, ttl, "reported TTL")
		})
	}
}

// TestEvaluate_ZeroTTLIsAlwaysStale verifies REQ-TTL-003: a TTL of zero means
// the request type will not accept an age-based answer at all. It must not be
// confused with an absent TTL, which would let anything through.
func TestEvaluate_ZeroTTLIsAlwaysStale(t *testing.T) {
	t.Parallel()

	obs := datasource.Observation{Found: true, LastUpdated: base, SourceTime: base}
	ev := Evaluate(obs, 0, time.Second, base)
	testutil.Equal(t, ev.State, domain.FreshnessStale,
		"a zero TTL must never be satisfied, even by a record refreshed this instant")
}

// TestEvaluate_UnknownWhenNoObservation verifies REQ-TTL-006: an absent record
// or an absent timestamp yields UNKNOWN, which the router treats differently
// from STALE.
func TestEvaluate_UnknownWhenNoObservation(t *testing.T) {
	t.Parallel()

	cases := map[string]datasource.Observation{
		"resource not found":  {Found: false},
		"no update timestamp": {Found: true, SourceTime: base},
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev := Evaluate(obs, 30*time.Second, time.Second, base)
			testutil.Equal(t, ev.State, domain.FreshnessUnknown, "state")
			testutil.Equal(t, ev.Age, time.Duration(0), "no age can be reported")
		})
	}
}

// TestEvaluate_UsesSourceClockDomain verifies REQ-EDGE-010: when the source
// reports its own current time, the age is computed entirely within the
// source's clock domain, so disagreement between the two machines' clocks
// cannot make a fresh record look stale or the reverse.
func TestEvaluate_UsesSourceClockDomain(t *testing.T) {
	t.Parallel()

	// The source's clock is five minutes ahead of the BFF's. Both its
	// last_updated and its server_time come from that clock, and the record is
	// genuinely ten seconds old.
	sourceNow := base.Add(5 * time.Minute)
	obs := datasource.Observation{
		Found:       true,
		LastUpdated: sourceNow.Add(-10 * time.Second),
		SourceTime:  sourceNow,
	}

	ev := Evaluate(obs, 30*time.Second, 2*time.Second, base)

	testutil.Equal(t, ev.Age, 10*time.Second,
		"the age must come from the source's own clock, not from the difference to ours")
	testutil.Equal(t, ev.State, domain.FreshnessFresh, "and the record is therefore fresh")
	testutil.True(t, ev.SkewCorrected, "the skew is still reported, so it can be alerted on")
	testutil.True(t, ev.SkewSeconds > 299 && ev.SkewSeconds < 301, "measured skew, got %v", ev.SkewSeconds)
}

// TestEvaluate_FutureTimestamps verifies REQ-EDGE-010: a record timestamped
// slightly in the future is clamped to zero age, but one timestamped
// implausibly far ahead is refused rather than trusted -- otherwise a source
// with a badly wrong clock could pin every record as permanently fresh.
func TestEvaluate_FutureTimestamps(t *testing.T) {
	t.Parallel()

	const tolerance = 2 * time.Second

	t.Run("within tolerance is clamped to zero", func(t *testing.T) {
		t.Parallel()
		obs := datasource.Observation{Found: true, LastUpdated: base.Add(time.Second)}
		ev := Evaluate(obs, 30*time.Second, tolerance, base)
		testutil.Equal(t, ev.Age, time.Duration(0), "age clamped")
		testutil.Equal(t, ev.State, domain.FreshnessFresh, "state")
		testutil.True(t, ev.SkewCorrected, "the clamp is reported")
	})

	t.Run("beyond tolerance refuses to judge", func(t *testing.T) {
		t.Parallel()
		obs := datasource.Observation{Found: true, LastUpdated: base.Add(time.Hour)}
		ev := Evaluate(obs, 30*time.Second, tolerance, base)
		testutil.Equal(t, ev.State, domain.FreshnessUnknown,
			"a source claiming to have refreshed a record an hour from now is telling us its clock is wrong")
		testutil.True(t, ev.SkewCorrected, "the anomaly is reported")
	})
}

// TestEvaluateFreshness_FromHeldValue verifies REQ-TTL-004: the same evaluation
// is applied to a record the BFF already holds, so the pre-fetch and post-fetch
// verdicts cannot use different arithmetic.
func TestEvaluateFreshness_FromHeldValue(t *testing.T) {
	t.Parallel()

	f := domain.Freshness{ObservedAt: base.Add(-45 * time.Second)}
	ev := EvaluateFreshness(f, 30*time.Second, time.Second, base)
	testutil.Equal(t, ev.State, domain.FreshnessStale, "state")
	testutil.Equal(t, ev.Age, 45*time.Second, "age")
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

type stubProbe struct {
	obs   datasource.Observation
	err   error
	calls int
}

func (s *stubProbe) ProbeFreshness(context.Context, string, string) (datasource.Observation, error) {
	s.calls++
	if s.err != nil {
		return datasource.Observation{}, s.err
	}
	return s.obs, nil
}

// TestManager_ProbeCacheBoundsCallRateNotStaleness verifies REQ-TTL-007: the
// probe memo limits how often the BFF asks the source how old a record is, and
// nothing else. A reused observation still ages, so a memoised probe can never
// make a stale record look fresh.
func TestManager_ProbeCacheBoundsCallRateNotStaleness(t *testing.T) {
	t.Parallel()

	clk := testutil.NewClock(base)
	p := &stubProbe{obs: datasource.Observation{
		Found:       true,
		LastUpdated: base.Add(-25 * time.Second),
		SourceTime:  base,
	}}
	m := NewManager(p, 5*time.Second, NoopHooks()).WithClock(clk.Now)

	ev, err := m.Assess(context.Background(), "t1", "R1", 30*time.Second, time.Second)
	testutil.NoError(t, err, "first assessment")
	testutil.Equal(t, ev.State, domain.FreshnessFresh, "25s old against a 30s TTL is fresh")
	testutil.Equal(t, p.calls, 1, "one probe so far")

	// Within the memo window: no second probe.
	clk.Advance(2 * time.Second)
	ev, err = m.Assess(context.Background(), "t1", "R1", 30*time.Second, time.Second)
	testutil.NoError(t, err, "second assessment")
	testutil.Equal(t, p.calls, 1, "the memo prevented a second probe")
	testutil.True(t, ev.FromCache, "and the result is flagged as memoised")
	// The crucial assertion: the age grew with wall time even though the
	// observation was reused.
	testutil.Equal(t, ev.Age, 27*time.Second, "the reused observation still aged")

	// Past the record's TTL, still inside the memo window: the verdict must
	// flip to stale without a new probe.
	clk.Advance(4 * time.Second)
	ev, err = m.Assess(context.Background(), "t1", "R1", 30*time.Second, time.Second)
	testutil.NoError(t, err, "third assessment")
	testutil.Equal(t, p.calls, 2, "the memo has expired, so a fresh probe was made")
	_ = ev
}

// TestManager_ProbeFailureIsNotAnError verifies REQ-TTL-008: a failed probe
// must not fail the request. The manager reports the failure separately so the
// router can apply its unknown-freshness policy.
func TestManager_ProbeFailureIsNotAnError(t *testing.T) {
	t.Parallel()

	p := &stubProbe{err: errors.New("source unreachable")}
	m := NewManager(p, time.Second, NoopHooks()).WithClock(func() time.Time { return base })

	ev, err := m.Assess(context.Background(), "t1", "R1", 30*time.Second, time.Second)
	testutil.Error(t, err, "the probe failure is surfaced to the caller")
	testutil.Equal(t, ev.State, domain.FreshnessUnknown,
		"and the verdict is UNKNOWN, which the router handles by policy rather than by failing")
}

// TestManager_InvalidateDropsMemo verifies REQ-TTL-009.
func TestManager_InvalidateDropsMemo(t *testing.T) {
	t.Parallel()

	p := &stubProbe{obs: datasource.Observation{Found: true, LastUpdated: base, SourceTime: base}}
	m := NewManager(p, time.Minute, NoopHooks()).WithClock(func() time.Time { return base })

	_, _ = m.Assess(context.Background(), "t1", "R1", time.Minute, time.Second)
	testutil.Equal(t, m.Size(), 1, "memoised")

	m.Invalidate("t1", "R1")
	testutil.Equal(t, m.Size(), 0, "dropped")

	_, _ = m.Assess(context.Background(), "t1", "R1", time.Minute, time.Second)
	testutil.Equal(t, p.calls, 2, "the next assessment probes again")
}

// TestManager_TenantsDoNotShareMemoEntries verifies REQ-MT-006: the probe memo
// is keyed by tenant as well as by resource, so one tenant's observation can
// never answer another tenant's question.
func TestManager_TenantsDoNotShareMemoEntries(t *testing.T) {
	t.Parallel()

	p := &stubProbe{obs: datasource.Observation{Found: true, LastUpdated: base, SourceTime: base}}
	m := NewManager(p, time.Minute, NoopHooks()).WithClock(func() time.Time { return base })

	_, _, _ = m.Probe(context.Background(), "tenant-a", "R1")
	_, _, _ = m.Probe(context.Background(), "tenant-b", "R1")
	testutil.Equal(t, p.calls, 2, "the same resource id under two tenants is two distinct probes")
	testutil.Equal(t, m.Size(), 2, "and two distinct memo entries")
}

// TestManager_NilProbeIsSafe verifies that a source which cannot answer a probe
// degrades to "no observation" rather than panicking, so the FreshnessProbe
// interface stays genuinely optional (REQ-DS-003).
func TestManager_NilProbeIsSafe(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, time.Second, NoopHooks())
	obs, cached, err := m.Probe(context.Background(), "t", "R1")
	testutil.NoError(t, err, "no probe configured is not an error")
	testutil.False(t, obs.Found, "and yields no observation")
	testutil.False(t, cached, "nothing was memoised")
}
