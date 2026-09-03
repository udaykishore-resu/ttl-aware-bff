// Package testutil holds the assertions, fakes and clocks the test suite
// shares.
//
// There is no third-party assertion library here on purpose. The whole service
// has exactly the dependencies it needs to talk to its sources and emit
// telemetry; adding one for `assert.Equal` would mean every consumer of this
// module inherits it. What follows is about eighty lines and covers everything
// the tests actually assert.
package testutil

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/pkg/errs"
)

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// Equal fails the test unless got and want are deeply equal.
func Equal[T any](t testing.TB, got, want T, msg string, args ...any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf(prefix(msg, args...)+"\n  got:  %#v\n  want: %#v", got, want)
	}
}

// NotEqual fails the test when got and want are deeply equal.
func NotEqual[T any](t testing.TB, got, want T, msg string, args ...any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		t.Fatalf(prefix(msg, args...)+"\n  both were: %#v", got)
	}
}

// True fails the test unless cond holds.
func True(t testing.TB, cond bool, msg string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatal(prefix(msg, args...))
	}
}

// False fails the test when cond holds.
func False(t testing.TB, cond bool, msg string, args ...any) {
	t.Helper()
	if cond {
		t.Fatal(prefix(msg, args...))
	}
}

// NoError fails the test when err is non-nil.
func NoError(t testing.TB, err error, msg string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(prefix(msg, args...)+"\n  unexpected error: %v", err)
	}
}

// Error fails the test when err is nil.
func Error(t testing.TB, err error, msg string, args ...any) {
	t.Helper()
	if err == nil {
		t.Fatal(prefix(msg, args...) + "\n  expected an error, got nil")
	}
}

// ErrCode fails the test unless err carries the expected taxonomy code.
func ErrCode(t testing.TB, err error, want errs.Code, msg string, args ...any) {
	t.Helper()
	got := errs.CodeOf(err)
	if got != want {
		t.Fatalf(prefix(msg, args...)+"\n  got code:  %s\n  want code: %s\n  error: %v", got, want, err)
	}
}

// WithinDuration fails the test unless got is within tolerance of want.
func WithinDuration(t testing.TB, got, want, tolerance time.Duration, msg string, args ...any) {
	t.Helper()
	d := got - want
	if d < 0 {
		d = -d
	}
	if d > tolerance {
		t.Fatalf(prefix(msg, args...)+"\n  got:  %s\n  want: %s (+/- %s)", got, want, tolerance)
	}
}

func prefix(msg string, args ...any) string {
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock is a manually advanced clock. Every time-dependent test uses one, so
// that no test sleeps and none is flaky under load.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock fixed at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute instant.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// ---------------------------------------------------------------------------
// Fake data sources
// ---------------------------------------------------------------------------

// FakeOperational is a programmable OperationalRepository.
type FakeOperational struct {
	mu sync.Mutex

	Resources map[string]*domain.Resource
	// Observation returned by the probe, keyed by resource id.
	Observations map[string]datasource.Observation

	// Errors, when set for a method name, are returned instead of data.
	Errors map[string]error
	// Delay is added to every call, so timeout behaviour can be tested.
	Delay time.Duration
	// Available reports the source's health.
	Available bool
	// Detail is the health detail string.
	Detail string

	// Calls counts invocations per method, for asserting that the BFF did NOT
	// call a source it was not supposed to call.
	Calls map[string]int
}

// NewFakeOperational returns an empty, healthy fake.
func NewFakeOperational() *FakeOperational {
	return &FakeOperational{
		Resources:    map[string]*domain.Resource{},
		Observations: map[string]datasource.Observation{},
		Errors:       map[string]error{},
		Calls:        map[string]int{},
		Available:    true,
		Detail:       "HEALTHY",
	}
}

// CallCount reports how many times a method was invoked.
func (f *FakeOperational) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[method]
}

// SetError programs a method to fail.
func (f *FakeOperational) SetError(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Errors[method] = err
}

// SetHealth programs the reported health.
func (f *FakeOperational) SetHealth(available bool, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Available, f.Detail = available, detail
}

func (f *FakeOperational) record(method string) error {
	f.mu.Lock()
	f.Calls[method]++
	err := f.Errors[method]
	delay := f.Delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return err
}

// ProbeFreshness implements datasource.FreshnessProbe.
func (f *FakeOperational) ProbeFreshness(_ context.Context, _, resourceID string) (datasource.Observation, error) {
	if err := f.record("ProbeFreshness"); err != nil {
		return datasource.Observation{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	obs, ok := f.Observations[resourceID]
	if !ok {
		return datasource.Observation{Found: false}, nil
	}
	return obs, nil
}

// GetResource implements datasource.OperationalRepository.
func (f *FakeOperational) GetResource(_ context.Context, tenantID, resourceID string, _ datasource.ReadOptions) (*domain.Resource, domain.Freshness, error) {
	if err := f.record("GetResource"); err != nil {
		return nil, domain.Freshness{}, err
	}
	return f.lookup(tenantID, resourceID)
}

// GetResourceState implements datasource.OperationalRepository.
func (f *FakeOperational) GetResourceState(_ context.Context, tenantID, resourceID string) (*domain.Resource, domain.Freshness, error) {
	if err := f.record("GetResourceState"); err != nil {
		return nil, domain.Freshness{}, err
	}
	return f.lookup(tenantID, resourceID)
}

func (f *FakeOperational) lookup(tenantID, resourceID string) (*domain.Resource, domain.Freshness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.Resources[resourceID]
	if !ok || (tenantID != "" && r.TenantID != "" && r.TenantID != tenantID) {
		return nil, domain.Freshness{}, errs.ErrNotFound
	}
	cp := *r
	return &cp, domain.Freshness{
		State:      domain.FreshnessUnknown,
		ObservedAt: r.ObservedAt,
		Source:     domain.SourceOperational,
	}, nil
}

// BatchGetResources implements datasource.OperationalRepository.
func (f *FakeOperational) BatchGetResources(_ context.Context, tenantID string, ids []string) ([]domain.Resource, error) {
	if err := f.record("BatchGetResources"); err != nil {
		return nil, err
	}
	out := make([]domain.Resource, 0, len(ids))
	for _, id := range ids {
		if r, _, err := f.lookup(tenantID, id); err == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// Health implements datasource.OperationalRepository.
func (f *FakeOperational) Health(context.Context) datasource.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return datasource.Health{Available: f.Available, Detail: f.Detail, CheckedAt: time.Now()}
}

// Close implements datasource.OperationalRepository.
func (f *FakeOperational) Close() error { return nil }

// FakeExecution is a programmable ExecutionRepository.
type FakeExecution struct {
	mu sync.Mutex

	Latest  map[string]*domain.Execution
	ByID    map[string]*domain.Execution
	History map[string]*domain.ExecutionList

	Errors    map[string]error
	Delay     time.Duration
	Available bool
	Detail    string
	Calls     map[string]int
}

// NewFakeExecution returns an empty, healthy fake.
func NewFakeExecution() *FakeExecution {
	return &FakeExecution{
		Latest:    map[string]*domain.Execution{},
		ByID:      map[string]*domain.Execution{},
		History:   map[string]*domain.ExecutionList{},
		Errors:    map[string]error{},
		Calls:     map[string]int{},
		Available: true,
		Detail:    "HEALTHY",
	}
}

// CallCount reports how many times a method was invoked.
func (f *FakeExecution) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[method]
}

// SetError programs a method to fail.
func (f *FakeExecution) SetError(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Errors[method] = err
}

// SetHealth programs the reported health.
func (f *FakeExecution) SetHealth(available bool, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Available, f.Detail = available, detail
}

func (f *FakeExecution) record(method string) error {
	f.mu.Lock()
	f.Calls[method]++
	err := f.Errors[method]
	delay := f.Delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return err
}

// GetLatestExecution implements datasource.ExecutionRepository.
func (f *FakeExecution) GetLatestExecution(_ context.Context, _, resourceID string) (*domain.Execution, error) {
	if err := f.record("GetLatestExecution"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.Latest[resourceID]
	if !ok {
		return nil, errs.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

// GetExecution implements datasource.ExecutionRepository.
func (f *FakeExecution) GetExecution(_ context.Context, _, executionID string, _ datasource.ReadOptions) (*domain.Execution, error) {
	if err := f.record("GetExecution"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.ByID[executionID]
	if !ok {
		return nil, errs.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

// ListExecutions implements datasource.ExecutionRepository.
func (f *FakeExecution) ListExecutions(_ context.Context, _, resourceID string, _ datasource.PageRequest, _ datasource.ReadOptions) (*domain.ExecutionList, error) {
	if err := f.record("ListExecutions"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.History[resourceID]
	if !ok {
		return &domain.ExecutionList{ResourceID: resourceID}, nil
	}
	cp := *l
	return &cp, nil
}

// Health implements datasource.ExecutionRepository.
func (f *FakeExecution) Health(context.Context) datasource.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return datasource.Health{Available: f.Available, Detail: f.Detail, CheckedAt: time.Now()}
}

// Close implements datasource.ExecutionRepository.
func (f *FakeExecution) Close() error { return nil }

// Compile-time proof that the fakes satisfy the ports. If a port changes, this
// breaks here rather than in every test that uses a fake.
var (
	_ datasource.OperationalRepository = (*FakeOperational)(nil)
	_ datasource.ExecutionRepository   = (*FakeExecution)(nil)
)
