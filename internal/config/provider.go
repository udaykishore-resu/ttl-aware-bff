package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Provider hands out the current configuration and, when watching a file,
// swaps it atomically as the file changes.
//
// Two properties matter and both are deliberate:
//
//   - Readers never block. Get() is a single atomic load, so the hot path pays
//     nothing for the ability to reconfigure.
//   - A reload that fails validation is refused and the previous configuration
//     stays in force. A typo in a ConfigMap must not take the service down
//     (REQ-CFG-006).
//
// Change detection is by content hash rather than mtime because Kubernetes
// ConfigMap updates land as an atomic symlink swap, which does not always move
// the mtime of the path the process opened.
type Provider struct {
	current atomic.Pointer[Config]

	path     string
	interval time.Duration
	log      *slog.Logger

	mu        sync.Mutex
	lastHash  string
	observers []func(old, new *Config)

	reloads  atomic.Int64
	failures atomic.Int64
}

// NewProvider returns a Provider serving a fixed configuration.
func NewProvider(cfg Config) *Provider {
	p := &Provider{}
	p.current.Store(&cfg)
	return p
}

// NewFileProvider loads path and returns a Provider that reloads it every
// interval. An interval of zero disables watching.
func NewFileProvider(path string, interval time.Duration, log *slog.Logger) (*Provider, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Provider{path: path, interval: interval, log: log}
	p.current.Store(&cfg)
	p.lastHash = hashFile(path)
	return p, nil
}

// Get returns the current configuration. The returned pointer is immutable:
// callers must not modify it.
func (p *Provider) Get() *Config { return p.current.Load() }

// OnChange registers a callback invoked after a successful reload. Callbacks
// run synchronously on the watch goroutine and must not block for long.
func (p *Provider) OnChange(fn func(old, new *Config)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observers = append(p.observers, fn)
}

// Watch blocks until ctx is done, reloading the file on change. It is a no-op
// when the Provider was built without a path or with a zero interval.
func (p *Provider) Watch(ctx context.Context) {
	if p.path == "" || p.interval <= 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.reloadIfChanged()
		}
	}
}

// Reload forces a reload regardless of whether the content changed. Exposed so
// an admin endpoint or a SIGHUP handler can trigger it.
func (p *Provider) Reload() error {
	cfg, err := Load(p.path)
	if err != nil {
		p.failures.Add(1)
		return err
	}
	old := p.current.Swap(&cfg)
	p.reloads.Add(1)
	p.notify(old, &cfg)
	return nil
}

// Stats reports reload counters for the admin surface and metrics.
func (p *Provider) Stats() (reloads, failures int64) {
	return p.reloads.Load(), p.failures.Load()
}

func (p *Provider) reloadIfChanged() {
	h := hashFile(p.path)
	if h == "" {
		return
	}
	p.mu.Lock()
	unchanged := h == p.lastHash
	p.mu.Unlock()
	if unchanged {
		return
	}

	cfg, err := Load(p.path)
	if err != nil {
		p.failures.Add(1)
		// Record the hash anyway so a persistently broken file is not
		// re-parsed and re-logged every tick.
		p.mu.Lock()
		p.lastHash = h
		p.mu.Unlock()
		p.log.Error("configuration reload rejected; keeping previous configuration",
			slog.String("path", p.path), slog.String("error", err.Error()))
		return
	}

	old := p.current.Swap(&cfg)
	p.mu.Lock()
	p.lastHash = h
	p.mu.Unlock()
	p.reloads.Add(1)
	p.log.Info("configuration reloaded",
		slog.String("path", p.path),
		slog.Int("request_types", len(cfg.Routing.RequestTypes)),
		slog.Int("tenant_overrides", len(cfg.Tenants)))
	p.notify(old, &cfg)
}

func (p *Provider) notify(old, new *Config) {
	p.mu.Lock()
	obs := make([]func(old, new *Config), len(p.observers))
	copy(obs, p.observers)
	p.mu.Unlock()
	for _, fn := range obs {
		fn(old, new)
	}
}

func hashFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
