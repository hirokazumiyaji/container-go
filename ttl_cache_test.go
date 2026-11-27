package container

import (
	"sync"
	"testing"
	"time"
)

func TestTTLCacheExpiresOnRead(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newTTLCache[string, string](time.Minute, 0)
	c.now = func() time.Time { return now }

	c.store("key", "value")
	if value, ok := c.get("key"); !ok || value != "value" {
		t.Fatalf("get before expiry = (%q, %t), want (%q, true)", value, ok, "value")
	}

	now = now.Add(time.Minute)
	if _, ok := c.get("key"); ok {
		t.Fatal("get at expiry returned a hit")
	}
	if got := c.size(); got != 0 {
		t.Errorf("size = %d after an expired entry was read, want 0", got)
	}
}

func TestTTLCacheStorePurgesExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newTTLCache[string, string](time.Minute, 0)
	c.now = func() time.Time { return now }

	c.store("stale", "value")
	now = now.Add(2 * time.Minute)
	c.store("fresh", "value")

	if got := c.size(); got != 1 {
		t.Errorf("size = %d after store purged expired entries, want 1", got)
	}
	if _, ok := c.get("fresh"); !ok {
		t.Error("fresh entry missing after store")
	}
}

func TestTTLCacheEvictsEarliestExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newTTLCache[string, string](time.Minute, 2)
	c.now = func() time.Time { return now }

	c.store("first", "one")
	now = now.Add(time.Second)
	c.store("second", "two")
	now = now.Add(time.Second)
	// Refreshing an existing key must not trigger capacity eviction.
	c.store("first", "one-refreshed")
	if _, ok := c.get("second"); !ok {
		t.Fatal("refreshing an existing key evicted another entry")
	}

	c.store("third", "three")
	if _, ok := c.get("second"); ok {
		t.Error("earliest-expiring entry survived capacity eviction")
	}
	if value, ok := c.get("first"); !ok || value != "one-refreshed" {
		t.Errorf("refreshed entry = (%q, %t), want it retained", value, ok)
	}
	if value, ok := c.get("third"); !ok || value != "three" {
		t.Errorf("just-stored entry = (%q, %t), want it retained", value, ok)
	}
}

func TestTTLCacheForgetRemovesOnlyThatKey(t *testing.T) {
	c := newTTLCache[string, string](time.Minute, 0)
	c.store("first", "one")
	c.store("second", "two")

	c.forget("first")
	if _, ok := c.get("first"); ok {
		t.Error("forgotten key still present")
	}
	if _, ok := c.get("second"); !ok {
		t.Error("forget removed an unrelated key")
	}
}

func TestTTLCacheDisabledAndNilAreInert(t *testing.T) {
	for name, c := range map[string]*ttlCache[string, string]{
		"zero ttl":     newTTLCache[string, string](0, 1),
		"negative ttl": newTTLCache[string, string](-time.Second, 1),
		"nil":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			c.store("key", "value")
			if _, ok := c.get("key"); ok {
				t.Error("disabled cache reported a hit")
			}
			c.forget("key")
			if c != nil && c.size() != 0 {
				t.Errorf("disabled cache holds %d entries, want 0", c.size())
			}
		})
	}
}

func TestTTLCacheConcurrentAccess(t *testing.T) {
	c := newTTLCache[string, string](time.Minute, 8)
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.store("shared", "value")
			c.get("shared")
			c.store("own", "value")
			c.forget("own")
			_ = c.size()
		}()
	}
	wg.Wait()
}
