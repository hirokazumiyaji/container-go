package container

import (
	"sync"
	"time"
)

// imageCache remembers that an image is present in a backend's local
// store, so a later Run does not have to ask the daemon again.
//
// Only positive answers are stored. A "missing" answer is deliberately
// not cached: the next operation pulls the image, and a cached absence
// would have to be invalidated by that pull. Since an image that is
// present stays present until something external removes it, caching
// presence is the direction that is cheap to reason about, and the TTL
// bounds the cost of the mistake.
//
// The zero value is unusable; construct it with newImageCache. A zero TTL
// means every lookup misses.
type imageCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry

	// now is the clock, injectable so the expiry behavior is testable
	// without sleeping.
	now func() time.Time
	// ttl is how long an entry stays valid.
	ttl time.Duration
}

// newImageCache returns a cache with the given entry lifetime. A ttl of
// zero or less yields a cache that never hits.
func newImageCache(ttl time.Duration) *imageCache {
	return &imageCache{
		entries: map[string]time.Time{},
		now:     time.Now,
		ttl:     ttl,
	}
}

// enabled reports whether the cache can hold entries at all.
func (c *imageCache) enabled() bool {
	return c != nil && c.ttl > 0
}

// key identifies one cached presence: one backend, one image, one
// platform variant. Same image on different platforms is a different
// entry, and the same image on a different backend is a different entry,
// because the two stores are independent.
func imageCacheKey(eng engine, image, platform string) string {
	return eng.name() + "\x00" + image + "\x00" + platform
}

// seen reports whether a fresh entry exists for key.
func (c *imageCache) seen(eng engine, image, platform string) bool {
	if !c.enabled() {
		return false
	}
	key := imageCacheKey(eng, image, platform)
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.entries[key]
	if !ok {
		return false
	}
	if !c.now().Before(expiry) {
		// Expired. Drop it so the map does not grow without bound when
		// an image is looked up repeatedly past its TTL.
		delete(c.entries, key)
		return false
	}
	return true
}

// remember records that the image is present.
func (c *imageCache) remember(eng engine, image, platform string) {
	if !c.enabled() {
		return
	}
	key := imageCacheKey(eng, image, platform)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = c.now().Add(c.ttl)
}

// forget drops any entry for key. Called after a pull, so that a pull of
// an image the cache had recorded as absent (or, defensively, present)
// leaves the cache describing the state after the pull.
func (c *imageCache) forget(eng engine, image, platform string) {
	if !c.enabled() {
		return
	}
	key := imageCacheKey(eng, image, platform)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// size reports how many entries are held. Used by tests to observe
// expiry without reaching into the map.
func (c *imageCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
