package container

import "time"

// Bound long-lived clients even when all cached answers are still fresh.
const imageCacheLimit = 256

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
// The bounded TTL map is the shared ttlCache; this type adds the
// backend/image/platform key. The zero value is unusable; construct it
// with newImageCache. A zero TTL means every lookup misses, and a nil
// cache is inert.
type imageCache struct {
	*ttlCache[string, imageIdentity]
}

// newImageCache returns a cache with the given entry lifetime. A ttl of
// zero or less yields a cache that never hits.
func newImageCache(ttl time.Duration) *imageCache {
	return &imageCache{ttlCache: newTTLCache[string, imageIdentity](ttl, imageCacheLimit)}
}

// enabled reports whether the cache can hold entries at all.
func (c *imageCache) enabled() bool {
	return c != nil && c.ttlCache.enabled()
}

// key identifies one cached presence: one backend, one image store
// (Docker client-config env for Docker), one image, one platform
// variant. Same image on different platforms or daemons is a different
// entry, because those stores are independent.
func imageCacheKey(eng engine, image, platform string) string {
	return eng.name() + "\x00" + eng.imageStoreID() + "\x00" + image + "\x00" + platform
}

// get returns the cached image identity if a fresh entry exists for key.
func (c *imageCache) get(eng engine, image, platform string) (imageIdentity, bool) {
	if !c.enabled() {
		return imageIdentity{}, false
	}
	return c.ttlCache.get(imageCacheKey(eng, image, platform))
}

// seen reports whether a fresh entry exists for key.
func (c *imageCache) seen(image, platform string) bool { //nolint:unparam // platform is part of the cache key contract
	_, ok := c.get(dockerEngine{}, image, platform)
	return ok
}

// remember records that the image is present. While inserting it also
// drops expired entries and evicts the earliest-expiring entry at capacity.
// A long-lived Option used with changing
// image references does not retain dead keys forever; seen() already
// drops an expired key on lookup, but keys that are never queried again
// would otherwise linger until the Option itself is discarded.
func (c *imageCache) remember(image, platform string) { //nolint:unparam // platform is part of the cache key contract
	c.rememberIdentity(dockerEngine{}, image, platform, imageIdentity{reference: image})

}

// rememberIdentity records that the image is present along with its
// resolved identity.
func (c *imageCache) rememberIdentity(eng engine, image, platform string, identity imageIdentity) {
	if !c.enabled() {
		return
	}
	c.store(imageCacheKey(eng, image, platform), identity)
}

// forget drops any entry for key. Called after a pull, so that a pull of
// an image the cache had recorded as absent (or, defensively, present)
// leaves the cache describing the state after the pull.
func (c *imageCache) forget(eng engine, image, platform string) {
	if !c.enabled() {
		return
	}
	c.ttlCache.forget(imageCacheKey(eng, image, platform))
}
