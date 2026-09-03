package cache

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/domain"
)

// Memory is a bounded, LRU-evicted in-process cache. It serves two roles:
//
//   - the L1 tier in front of Redis, absorbing the repeated reads a UI makes
//     while a user sits on one screen;
//   - the whole cache in single-instance and test deployments.
//
// Expired entries are removed lazily on read and opportunistically on write,
// which avoids a sweeper goroutine per instance. The bound is on entry count
// rather than bytes because entries here are small and uniform, and counting
// bytes would cost more than it saves.
type Memory struct {
	mu       sync.Mutex
	maxItems int
	items    map[string]*list.Element
	lru      *list.List
	now      func() time.Time
}

type memItem struct {
	key       string
	entry     *Entry
	expiresAt time.Time
}

// NewMemory builds an in-process cache holding at most maxItems entries.
func NewMemory(maxItems int) *Memory {
	if maxItems < 1 {
		maxItems = 1024
	}
	return &Memory{
		maxItems: maxItems,
		items:    make(map[string]*list.Element, maxItems),
		lru:      list.New(),
		now:      time.Now,
	}
}

// WithClock injects a clock for deterministic tests.
func (m *Memory) WithClock(now func() time.Time) *Memory {
	m.now = now
	return m
}

// Name implements Cache.
func (m *Memory) Name() string { return "memory" }

// Get implements Cache.
func (m *Memory) Get(_ context.Context, key string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, ErrMiss
	}
	it := el.Value.(*memItem)
	if m.now().After(it.expiresAt) {
		m.removeLocked(el)
		return nil, ErrMiss
	}
	m.lru.MoveToFront(el)
	// The returned entry must share nothing mutable with the stored one. A
	// shallow copy protects the scalar fields but leaves Payload and the
	// slices/maps aliased, so a caller writing into the payload it was handed
	// would corrupt what the next reader sees -- a bug that only appears under
	// concurrency and is very hard to trace back here.
	return cloneEntry(it.entry), nil
}

// Set implements Cache.
func (m *Memory) Set(_ context.Context, key string, e *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	cp := cloneEntry(e)
	m.mu.Lock()
	defer m.mu.Unlock()
	exp := m.now().Add(ttl)
	if el, ok := m.items[key]; ok {
		it := el.Value.(*memItem)
		it.entry, it.expiresAt = cp, exp
		m.lru.MoveToFront(el)
		return nil
	}
	el := m.lru.PushFront(&memItem{key: key, entry: cp, expiresAt: exp})
	m.items[key] = el
	for m.lru.Len() > m.maxItems {
		if back := m.lru.Back(); back != nil {
			m.removeLocked(back)
		}
	}
	return nil
}

// Delete implements Cache.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.removeLocked(el)
	}
	return nil
}

// DeletePrefix implements Cache.
func (m *Memory) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, el := range m.items {
		if strings.HasPrefix(k, prefix) {
			m.removeLocked(el)
		}
	}
	return nil
}

// Close implements Cache.
func (m *Memory) Close() error { return nil }

// Len reports the current entry count, for tests and metrics.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lru.Len()
}

// cloneEntry deep-copies everything a caller could mutate.
func cloneEntry(e *Entry) *Entry {
	cp := *e
	if e.Payload != nil {
		cp.Payload = make([]byte, len(e.Payload))
		copy(cp.Payload, e.Payload)
	}
	if e.Sources != nil {
		cp.Sources = make([]domain.SourceKind, len(e.Sources))
		copy(cp.Sources, e.Sources)
	}
	if e.Warnings != nil {
		cp.Warnings = make([]domain.Warning, len(e.Warnings))
		copy(cp.Warnings, e.Warnings)
	}
	if e.Provenance != nil {
		cp.Provenance = make(map[string]domain.SourceKind, len(e.Provenance))
		for k, v := range e.Provenance {
			cp.Provenance[k] = v
		}
	}
	return &cp
}

func (m *Memory) removeLocked(el *list.Element) {
	it := el.Value.(*memItem)
	delete(m.items, it.key)
	m.lru.Remove(el)
}
