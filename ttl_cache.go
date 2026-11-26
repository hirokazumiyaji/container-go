package container

import (
	"sync"
	"time"
)

// ttlCache is the bounded, expiring map shared by dockerMetadataCache and
// imageCache. It stores successful answers only, drops an entry the first
// time it is read after its expiry, and evicts the earliest-expiring entry
// when a new key would exceed the capacity limit.
//
// The zero value is unusable; construct it with newTTLCache. A non-positive
// ttl makes every lookup miss and every store a no-op, which is how a cache
// option of zero disables caching without special-casing every caller. A
// non-positive limit disables eviction.
//
// A nil *ttlCache is inert: get misses, store and forget do nothing. That
// keeps an optional cache field usable without a nil check at every call.
type ttlCache[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]ttlCacheEntry[V]
	// nextExpiry is the earliest expiry among entries, or the zero time when
	// no entry is known to expire. It lets store skip a full sweep until an
	// entry can actually be stale.
	nextExpiry time.Time

	// now and ttl are injectable so expiry is testable without sleeping.
	now func() time.Time
	// ttl is how long an entry stays valid.
	ttl time.Duration
	// limit is the maximum number of live entries.
	limit int
}

type ttlCacheEntry[V any] struct {
	value  V
	expiry time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration, limit int) *ttlCache[K, V] {
	return &ttlCache[K, V]{
		entries: map[K]ttlCacheEntry[V]{},
		now:     time.Now,
		ttl:     ttl,
		limit:   limit,
	}
}

// enabled reports whether the cache can hold entries at all.
func (c *ttlCache[K, V]) enabled() bool {
	return c != nil && c.ttl > 0
}

func (c *ttlCache[K, V]) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// get returns the cached value if a fresh entry exists for key.
func (c *ttlCache[K, V]) get(key K) (V, bool) {
	var zero V
	if !c.enabled() {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !c.currentTime().Before(entry.expiry) {
		// Expired. Drop it so the map does not grow without bound when a
		// key is looked up repeatedly past its TTL.
		delete(c.entries, key)
		return zero, false
	}
	return entry.value, true
}

// store records value for key until it expires. While inserting it drops
// expired entries and evicts the earliest-expiring entry at capacity, so a
// long-lived cache does not retain dead keys that are never read again.
func (c *ttlCache[K, V]) store(key K, value V) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.currentTime()
	if !c.nextExpiry.IsZero() && !now.Before(c.nextExpiry) {
		c.purgeExpiredLocked(now)
	}
	if _, exists := c.entries[key]; !exists && c.limit > 0 && len(c.entries) >= c.limit {
		c.evictEarliestExpiryLocked()
	}
	expiry := now.Add(c.ttl)
	c.entries[key] = ttlCacheEntry[V]{value: value, expiry: expiry}
	if c.nextExpiry.IsZero() || expiry.Before(c.nextExpiry) {
		c.nextExpiry = expiry
	}
}

// forget drops any entry for key.
func (c *ttlCache[K, V]) forget(key K) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// size reports how many entries are held. Used by tests to observe expiry
// and capacity without reaching into the map.
func (c *ttlCache[K, V]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// purgeExpiredLocked drops entries whose expiry has passed and restores the
// earliest-expiry hint. It runs only when the hint says at least one entry
// may be stale.
func (c *ttlCache[K, V]) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiry) {
			delete(c.entries, key)
		}
	}
	c.recomputeNextExpiryLocked()
}

// evictEarliestExpiryLocked keeps a just-stored hot answer from being evicted
// at capacity: with one TTL per cache, the earliest expiry is the oldest
// insertion, which is the least likely answer a caller needs next.
func (c *ttlCache[K, V]) evictEarliestExpiryLocked() {
	var oldestKey K
	var oldestExpiry time.Time
	found := false
	for key, entry := range c.entries {
		if !found || entry.expiry.Before(oldestExpiry) {
			oldestKey, oldestExpiry = key, entry.expiry
			found = true
		}
	}
	if !found {
		return
	}
	delete(c.entries, oldestKey)
	c.recomputeNextExpiryLocked()
}

func (c *ttlCache[K, V]) recomputeNextExpiryLocked() {
	c.nextExpiry = time.Time{}
	for _, entry := range c.entries {
		if c.nextExpiry.IsZero() || entry.expiry.Before(c.nextExpiry) {
			c.nextExpiry = entry.expiry
		}
	}
}
