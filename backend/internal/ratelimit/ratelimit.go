// Package ratelimit provides fixed-window rate limiting backed by Redis, with
// an in-process fallback. Rate limiting is a protective control only; it is
// never relied upon for financial correctness.
package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter interface {
	// Allow records a hit for key and reports whether it is within limit
	// hits per window.
	Allow(ctx context.Context, key string, limit int, window time.Duration) bool
}

type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	count int
	reset time.Time
}

func NewMemory() *Memory { return &Memory{buckets: map[string]*bucket{}} }

func (m *Memory) Allow(_ context.Context, key string, limit int, window time.Duration) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.last) > time.Minute { // opportunistic cleanup
		for k, b := range m.buckets {
			if now.After(b.reset) {
				delete(m.buckets, k)
			}
		}
		m.last = now
	}
	b, ok := m.buckets[key]
	if !ok || now.After(b.reset) {
		b = &bucket{reset: now.Add(window)}
		m.buckets[key] = b
	}
	b.count++
	return b.count <= limit
}

// Redis uses INCR + PEXPIRE NX per window. On Redis errors it falls back to
// the in-memory limiter rather than failing open entirely.
type Redis struct {
	Client   *redis.Client
	Fallback *Memory
	Log      *slog.Logger
}

func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) bool {
	k := "rl:" + key + ":" + time.Now().Truncate(window).Format("20060102150405")
	pipe := r.Client.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.PExpire(ctx, k, window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		r.Log.Warn("redis rate limiter unavailable; using in-memory fallback", "err", err)
		return r.Fallback.Allow(ctx, key, limit, window)
	}
	return incr.Val() <= int64(limit)
}
