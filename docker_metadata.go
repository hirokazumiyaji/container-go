package container

import (
	"context"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	// dockerMetadataTTL bounds how long a successful Docker metadata lookup
	// may be reused. The key includes the resolved target, so switching
	// hosts, contexts, or client configuration never reads another daemon's
	// answer; the TTL only bounds a change on the same endpoint.
	dockerMetadataTTL = 30 * time.Second
	// dockerMetadataLimit bounds the number of entries one Client can
	// retain. A Client pins one target, so in practice it needs a handful;
	// the limit keeps a caller that constructs engines directly from
	// growing the map without bound.
	dockerMetadataLimit = 64
)

type dockerMetadataEntry struct {
	value  string
	expiry time.Time
}

// dockerMetadataCache collapses concurrent Docker metadata lookups with the
// same key and briefly reuses their successful results. It is owned by a
// Client (and carried by the handles that Client creates) rather than shared
// process-wide, so unrelated targets cannot observe each other's answers.
//
// Only successful lookups are stored. A failed or canceled lookup removes
// nothing because nothing was stored, and the next caller retries. A joined
// caller's context still bounds its wait; the leader's context bounds the CLI
// invocation itself.
type dockerMetadataCache struct {
	mu      sync.Mutex
	entries map[string]dockerMetadataEntry
	flights flightGroup[string]

	// now and ttl are injectable so expiry is testable without sleeping.
	now func() time.Time
	ttl time.Duration
	// limit is the maximum number of live entries; a non-positive limit
	// disables eviction.
	limit int
}

func newDockerMetadataCache(ttl time.Duration) *dockerMetadataCache {
	return &dockerMetadataCache{
		entries: map[string]dockerMetadataEntry{},
		now:     time.Now,
		ttl:     ttl,
		limit:   dockerMetadataLimit,
	}
}

func (c *dockerMetadataCache) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// lookup returns a cached successful value for key, or runs fn once for all
// concurrent callers with that key. A nil cache still runs fn, which keeps the
// process-global Run path working without a package-level cache.
func (c *dockerMetadataCache) lookup(ctx context.Context, key string, fn func(context.Context) (string, error)) (string, error) {
	if c == nil {
		return fn(ctx)
	}
	if value, ok := c.get(key); ok {
		return value, nil
	}
	return c.flights.do(ctx, key, func() (string, error) {
		// A leader may have populated the entry after this caller missed
		// the unlocked read but before it became the leader.
		if value, ok := c.get(key); ok {
			return value, nil
		}
		value, err := fn(ctx)
		if err != nil {
			return "", err
		}
		c.store(key, value)
		return value, nil
	})
}

func (c *dockerMetadataCache) get(key string) (string, bool) {
	if c.ttl <= 0 {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !c.currentTime().Before(entry.expiry) {
		delete(c.entries, key)
		return "", false
	}
	return entry.value, true
}

func (c *dockerMetadataCache) store(key, value string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.currentTime()
	for existing, entry := range c.entries {
		if !now.Before(entry.expiry) {
			delete(c.entries, existing)
		}
	}
	if c.limit > 0 && len(c.entries) >= c.limit {
		for existing := range c.entries {
			delete(c.entries, existing)
			break
		}
	}
	c.entries[key] = dockerMetadataEntry{value: value, expiry: now.Add(c.ttl)}
}

// dockerDefaultNetworkKey identifies one daemon default-network answer: the
// backend, the resolved target and client settings, and the executable that
// issued the query. The effective query timeout changes how long a lookup may
// take, not what a successful answer means, so it is intentionally excluded.
func dockerDefaultNetworkKey(eng engine, runner cli.Runner) string {
	return eng.name() + "\x00" + eng.imageStoreID() + "\x00" + dockerRunnerKey(runner)
}

// dockerRunnerKey distinguishes CLI executables without treating an opaque
// test runner as a new target. Custom runners own their environment and
// cannot be fingerprinted here; callers that need isolation use separate
// Clients.
func dockerRunnerKey(runner cli.Runner) string {
	er, ok := runner.(*cli.ExecRunner)
	if !ok {
		return ""
	}
	return er.Binary + "\x00" + er.Identity
}

// dockerDefaultNetworkCached resolves the daemon default network through the
// optional shared cache. A nil cache performs the lookup directly.
func dockerDefaultNetworkCached(ctx context.Context, cache *dockerMetadataCache, runner cli.Runner, eng engine, timeouts opTimeouts) (string, error) {
	if cache == nil {
		return dockerDefaultNetwork(ctx, runner, eng, timeouts)
	}
	return cache.lookup(ctx, dockerDefaultNetworkKey(eng, runner), func(ctx context.Context) (string, error) {
		return dockerDefaultNetwork(ctx, runner, eng, timeouts)
	})
}
