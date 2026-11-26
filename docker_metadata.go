package container

import (
	"context"
	"strconv"
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

// dockerMetadataCache collapses concurrent Docker metadata lookups with the
// same key and briefly reuses their successful results. It caches the daemon
// default network and the copy-out version capability. It is owned by a
// Client (and carried by the handles that Client creates) rather than shared
// process-wide, so unrelated targets cannot observe each other's answers.
//
// The bounded TTL map is the shared ttlCache; this type adds only the flight
// group that collapses a concurrent miss onto one CLI invocation.
//
// Only successful lookups are stored. A failed or canceled lookup removes
// nothing because nothing was stored, and the next caller retries. A joined
// caller's context still bounds its wait; the leader's context bounds the CLI
// invocation itself.
type dockerMetadataCache struct {
	*ttlCache[string, string]
	flights flightGroup[string]
}

func newDockerMetadataCache(ttl time.Duration) *dockerMetadataCache {
	return &dockerMetadataCache{
		ttlCache: newTTLCache[string, string](ttl, dockerMetadataLimit),
	}
}

// lookup returns a cached successful value for key, or runs fn once for all
// concurrent callers with that key. A nil cache still runs fn, which keeps the
// process-global Run path working without a package-level cache.
func (c *dockerMetadataCache) lookup(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (string, error)) (string, error) {
	if c == nil {
		return fn(ctx)
	}
	if value, ok := c.get(key); ok {
		return value, nil
	}
	if timeout <= 0 {
		timeout = queryTimeout
	}
	flightKey := key + "\x00query=" + strconv.FormatInt(int64(timeout), 10)
	return c.flights.do(ctx, flightKey, func() (string, error) {
		// A leader may have populated the entry after this caller missed
		// the unlocked read but before it became the leader.
		if value, ok := c.get(key); ok {
			return value, nil
		}
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		value, err := fn(lookupCtx)
		if err != nil {
			return "", err
		}
		c.store(key, value)
		return value, nil
	})
}

// dockerDefaultNetworkKey identifies one daemon default-network answer: the
// backend, the resolved target and client settings, and the executable that
// issued the query. The effective query timeout changes how long a lookup may
// take, not what a successful answer means, so cache entries omit it; flights
// append the timeout separately to isolate callers with different budgets.
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
	return cache.lookup(ctx, dockerDefaultNetworkKey(eng, runner), timeouts.query, func(ctx context.Context) (string, error) {
		return dockerDefaultNetwork(ctx, runner, eng, timeouts)
	})
}

// dockerCopyVersionKey identifies one daemon's copy-out version answer. It
// shares the target component of dockerDefaultNetworkKey, so every handle a
// Client creates on that target reuses one probe; the prefix keeps the value
// distinct from the default-network name stored in the same map.
func dockerCopyVersionKey(eng engine, runner cli.Runner) string {
	return "copy-out-version\x00" + dockerDefaultNetworkKey(eng, runner)
}

const dockerCopyVersionOK = "ok"

// dockerCopyFileFromContainerVersionCached verifies Docker's copy-out
// client/server versions through the optional shared cache. Only a successful
// check is stored, so a failed probe never poisons the next caller. A nil
// cache performs the check directly.
func dockerCopyFileFromContainerVersionCached(ctx context.Context, cache *dockerMetadataCache, runner cli.Runner, eng engine, timeouts opTimeouts) error {
	check := func(ctx context.Context) (string, error) {
		if err := eng.checkCopyFileFromContainerVersion(ctx, runner, timeouts); err != nil {
			return "", err
		}
		return dockerCopyVersionOK, nil
	}
	if cache == nil {
		_, err := check(ctx)
		return err
	}
	_, err := cache.lookup(ctx, dockerCopyVersionKey(eng, runner), timeouts.query, check)
	return err
}
