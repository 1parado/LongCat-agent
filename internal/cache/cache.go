// Package cache provides small, concurrency-safe in-memory caches used by
// request-scoped services. Entries are evicted by TTL and least-recent use.
package cache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value      V
	expiresAt  time.Time
	accessedAt time.Time
	accessSeq  uint64
}

type flight[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// Stats describes cache activity since creation.
type Stats struct {
	Size      int     `json:"size"`
	MaxItems  int     `json:"max_items"`
	Hits      uint64  `json:"hits"`
	Misses    uint64  `json:"misses"`
	Evictions uint64  `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
}

// Cache is a TTL cache with LRU-style eviction and concurrent load
// de-duplication. It is intentionally in-memory only.
type Cache[K comparable, V any] struct {
	mu        sync.Mutex
	entries   map[K]entry[V]
	inflight  map[K]*flight[V]
	ttl       time.Duration
	maxItems  int
	hits      uint64
	misses    uint64
	evictions uint64
	sequence  uint64
}

// New creates a cache. A non-positive TTL means entries do not expire; a
// non-positive maxItems means the cache is unbounded.
func New[K comparable, V any](ttl time.Duration, maxItems int) *Cache[K, V] {
	return &Cache[K, V]{entries: make(map[K]entry[V]), inflight: make(map[K]*flight[V]), ttl: ttl, maxItems: maxItems}
}

func (c *Cache[K, V]) expired(e entry[V], now time.Time) bool {
	return c.ttl > 0 && !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// Get returns a non-expired value.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	e, ok := c.entries[key]
	if !ok || c.expired(e, time.Now()) {
		c.misses++
		if ok {
			delete(c.entries, key)
		}
		return zero, false
	}
	e.accessedAt = time.Now()
	c.sequence++
	e.accessSeq = c.sequence
	c.entries[key] = e
	c.hits++
	return e.value, true
}

// Set stores a value and evicts the least recently used entries if needed.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.sequence++
	e := entry[V]{value: value, accessedAt: now, accessSeq: c.sequence}
	if c.ttl > 0 {
		e.expiresAt = now.Add(c.ttl)
	}
	c.entries[key] = e
	c.evictLocked()
}

func (c *Cache[K, V]) evictLocked() {
	now := time.Now()
	for key, e := range c.entries {
		if c.expired(e, now) {
			delete(c.entries, key)
		}
	}
	for c.maxItems > 0 && len(c.entries) > c.maxItems {
		var oldest K
		var oldestAt time.Time
		first := true
		var oldestSeq uint64
		for key, e := range c.entries {
			if first || e.accessedAt.Before(oldestAt) || (e.accessedAt.Equal(oldestAt) && e.accessSeq < oldestSeq) {
				oldest, oldestAt, oldestSeq, first = key, e.accessedAt, e.accessSeq, false
			}
		}
		delete(c.entries, oldest)
		c.evictions++
	}
}

// Delete removes a key.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Clear removes all cached values. In-flight loads are allowed to finish.
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	c.entries = make(map[K]entry[V])
	c.mu.Unlock()
}

// GetOrLoad returns a cached value or runs loader once for concurrent callers
// requesting the same key.
func (c *Cache[K, V]) GetOrLoad(key K, loader func() (V, error)) (V, error) {
	if value, ok := c.Get(key); ok {
		return value, nil
	}
	c.mu.Lock()
	if current, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-current.done
		return current.value, current.err
	}

	f := &flight[V]{done: make(chan struct{})}
	c.inflight[key] = f
	c.mu.Unlock()

	f.value, f.err = loader()
	if f.err == nil {
		c.Set(key, f.value)
	}
	c.mu.Lock()
	delete(c.inflight, key)
	close(f.done)
	c.mu.Unlock()
	return f.value, f.err
}

// Stats returns a point-in-time view of cache usage.
func (c *Cache[K, V]) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictLocked()
	total := c.hits + c.misses
	rate := 0.0
	if total > 0 {
		rate = float64(c.hits) / float64(total)
	}
	return Stats{Size: len(c.entries), MaxItems: c.maxItems, Hits: c.hits, Misses: c.misses, Evictions: c.evictions, HitRate: rate}
}
