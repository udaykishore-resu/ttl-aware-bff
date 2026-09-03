package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
)

// Redis is the L2, cross-instance cache tier.
//
// Notes on the choices here:
//
//   - Entries are JSON rather than gob or protobuf. The payload is already
//     JSON, the volume is low, and being able to read a key with redis-cli
//     during an incident is worth more than the bytes saved.
//   - Every operation carries its own short timeout derived from
//     configuration. A slow cache must never become a slow API: the caller
//     treats any error as a miss (fail-open).
//   - DeletePrefix uses SCAN with a bounded batch size, never KEYS.
type Redis struct {
	client  redis.UniversalClient
	timeout time.Duration
}

// NewRedis connects to Redis. It does not block on the connection: go-redis
// dials lazily, and a cache that cannot be reached must not stop the service
// from starting (REQ-CACHE-009).
func NewRedis(cfg config.RedisConfig) (*Redis, error) {
	opts := &redis.Options{
		Addr:         cfg.Addr,
		DB:           cfg.DB,
		Username:     cfg.Username,
		Password:     os.Getenv(cfg.PasswordEnv),
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdle,
		DialTimeout:  cfg.DialTimeout.D(),
		ReadTimeout:  cfg.ReadTimeout.D(),
		WriteTimeout: cfg.WriteTimeout.D(),
	}
	if cfg.TLS.Enabled {
		tc := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.TLS.InsecureSkipVerify, //nolint:gosec // operator-controlled, refused in prod config
			ServerName:         cfg.TLS.ServerName,
		}
		opts.TLSConfig = tc
	}
	timeout := cfg.ReadTimeout.D()
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	return &Redis{client: redis.NewClient(opts), timeout: timeout}, nil
}

// NewRedisWithClient wraps an existing client. Used by integration tests.
func NewRedisWithClient(c redis.UniversalClient, timeout time.Duration) *Redis {
	return &Redis{client: c, timeout: timeout}
}

// Name implements Cache.
func (r *Redis) Name() string { return "redis" }

// Ping verifies connectivity. Used by the readiness probe, which reports the
// cache as degraded rather than unready: the BFF works without a cache.
func (r *Redis) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.client.Ping(ctx).Err()
}

// Get implements Cache.
func (r *Redis) Get(ctx context.Context, key string) (*Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrMiss
		}
		return nil, fmt.Errorf("cache: redis get: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		// A corrupt or old-format entry is a miss, not an error: the request
		// simply falls through to the source and rewrites the key.
		return nil, ErrMiss
	}
	if e.SchemaVersion != EntrySchemaVersion {
		return nil, ErrMiss
	}
	return &e, nil
}

// Set implements Cache.
func (r *Redis) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	e.SchemaVersion = EntrySchemaVersion
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("cache: encode entry: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.client.Set(ctx, key, raw, ttl).Err(); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}

// Delete implements Cache.
func (r *Redis) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("cache: redis del: %w", err)
	}
	return nil
}

// scanBatch bounds how much work one SCAN iteration asks Redis to do.
const scanBatch = 256

// DeletePrefix implements Cache using SCAN + batched UNLINK.
func (r *Redis) DeletePrefix(ctx context.Context, prefix string) error {
	// A longer budget than a normal read: this is an administrative path.
	ctx, cancel := context.WithTimeout(ctx, 10*r.timeout)
	defer cancel()

	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, prefix+"*", scanBatch).Result()
		if err != nil {
			return fmt.Errorf("cache: redis scan: %w", err)
		}
		if len(keys) > 0 {
			if err := r.client.Unlink(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("cache: redis unlink: %w", err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cache: redis scan cancelled: %w", err)
		}
	}
}

// Close implements Cache.
func (r *Redis) Close() error { return r.client.Close() }

// AcquireLock takes a short-lived distributed lock, used for stampede
// protection across instances. It returns false without error when the lock is
// already held: that is the expected, non-exceptional case.
func (r *Redis) AcquireLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ok, err := r.client.SetNX(ctx, key+":lock", "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("cache: redis lock: %w", err)
	}
	return ok, nil
}

// ReleaseLock drops a lock acquired by AcquireLock.
func (r *Redis) ReleaseLock(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	_ = r.client.Del(ctx, key+":lock").Err()
}
