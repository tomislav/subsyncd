package provider

import (
	"context"
	"sync"
	"time"
)

// TTLCache keeps successful provider answers in memory so the episodes of one
// season share a single title search and subtitle listing. It is shared by
// adapters and holds provider metadata only, never credentials or URLs, and is
// deliberately lost on restart.
type TTLCache[V any] struct {
	mu       sync.Mutex
	clock    Clock
	ttl      time.Duration
	limit    int
	entries  map[string]cacheEntry[V]
	inFlight map[string]chan struct{}
	waiters  int
}

type cacheEntry[V any] struct {
	value  V
	stored time.Time
}

// NewTTLCache returns a cache whose entries expire after ttl and which keeps at
// most limit entries, evicting the oldest first.
func NewTTLCache[V any](clock Clock, ttl time.Duration, limit int) *TTLCache[V] {
	return &TTLCache[V]{clock: clock, ttl: ttl, limit: limit, entries: make(map[string]cacheEntry[V]), inFlight: make(map[string]chan struct{})}
}

// Load returns the cached value or runs fetch, letting concurrent callers for
// the same key share one fetch. fetch reports whether its answer may be cached.
// When the shared fetch fails or is uncacheable, a waiter retries and one of
// them fetches again, so one caller's cancellation never fails another.
func (c *TTLCache[V]) Load(ctx context.Context, key string, fetch func() (V, bool, error)) (V, error) {
	for {
		c.mu.Lock()
		if value, ok := c.lookupLocked(key); ok {
			c.mu.Unlock()
			return value, nil
		}
		done, busy := c.inFlight[key]
		if !busy {
			done = make(chan struct{})
			c.inFlight[key] = done
			c.mu.Unlock()
			value, cacheable, err := fetch()
			c.mu.Lock()
			if err == nil && cacheable {
				c.storeLocked(key, value)
			}
			delete(c.inFlight, key)
			close(done)
			c.mu.Unlock()
			return value, err
		}
		c.waiters++
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		c.mu.Lock()
		c.waiters--
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			var zero V
			return zero, err
		}
	}
}

// Waiting reports callers blocked on another caller's fetch.
func (c *TTLCache[V]) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiters
}

func (c *TTLCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(key)
}

func (c *TTLCache[V]) Put(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

func (c *TTLCache[V]) lookupLocked(key string) (V, bool) {
	entry, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !c.clock.Now().Before(entry.stored.Add(c.ttl)) {
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	return entry.value, true
}

func (c *TTLCache[V]) storeLocked(key string, value V) {
	now := c.clock.Now()
	c.entries[key] = cacheEntry[V]{value: value, stored: now}
	for len(c.entries) > c.limit {
		oldestKey, oldest := "", now
		for candidateKey, entry := range c.entries {
			if oldestKey == "" || entry.stored.Before(oldest) {
				oldestKey, oldest = candidateKey, entry.stored
			}
		}
		delete(c.entries, oldestKey)
	}
}
