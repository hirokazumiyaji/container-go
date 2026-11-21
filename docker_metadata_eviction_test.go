package container

import (
	"testing"
	"time"
)

func TestDockerMetadataCacheEvictsEarliestExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newDockerMetadataCache(time.Minute)
	c.now = func() time.Time { return now }
	c.limit = 2

	c.store("first", "one")
	now = now.Add(time.Second)
	c.store("second", "two")
	now = now.Add(time.Second)
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

func TestDockerMetadataCacheStorePurgesExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newDockerMetadataCache(time.Minute)
	c.now = func() time.Time { return now }

	c.store("stale", "value")
	now = now.Add(2 * time.Minute)
	c.store("fresh", "value")

	c.mu.Lock()
	_, stale := c.entries["stale"]
	size := len(c.entries)
	c.mu.Unlock()
	if stale {
		t.Error("expired entry survived a store")
	}
	if size != 1 {
		t.Errorf("cache size = %d, want 1", size)
	}
	if _, ok := c.get("fresh"); !ok {
		t.Error("fresh entry missing after store")
	}
}
