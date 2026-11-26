package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
// same key and briefly reuses their successful results. It caches the daemon
// default network and the copy-out version capability. It is owned by a
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
	// nextExpiry is the earliest expiry among entries, or the zero time when
	// no entry is known to expire. It lets store skip a full sweep until an
	// entry can actually be stale.
	nextExpiry time.Time
	flights    flightGroup[string]

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
func (c *dockerMetadataCache) lookup(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (string, error)) (string, error) {
	return c.lookupWithStore(ctx, key, timeout, func(ctx context.Context) (string, bool, error) {
		value, err := fn(ctx)
		return value, err == nil, err
	})
}

// lookupWithStore is lookup for a value that can be usable to its caller
// without being safe to reuse. keep reports whether the value may be cached;
// a rejected value still reaches the caller so it can report its own error.
// Only successful, reusable lookups are stored.
func (c *dockerMetadataCache) lookupWithStore(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (value string, keep bool, err error)) (string, error) {
	if c == nil {
		value, _, err := fn(ctx)
		return value, err
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
		value, keep, err := fn(lookupCtx)
		if err != nil {
			return "", err
		}
		if !keep {
			return value, nil
		}
		c.store(key, value)
		return value, nil
	})
}

// delete removes a cached answer that a consumer found unusable, so the next
// caller retries the lookup instead of reusing the failure until the TTL.
func (c *dockerMetadataCache) delete(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	c.recomputeNextExpiryLocked()
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
	if !c.nextExpiry.IsZero() && !now.Before(c.nextExpiry) {
		c.purgeExpiredLocked(now)
	}
	if _, exists := c.entries[key]; !exists && c.limit > 0 && len(c.entries) >= c.limit {
		c.evictEarliestExpiryLocked()
	}
	expiry := now.Add(c.ttl)
	c.entries[key] = dockerMetadataEntry{value: value, expiry: expiry}
	if c.nextExpiry.IsZero() || expiry.Before(c.nextExpiry) {
		c.nextExpiry = expiry
	}
}

// purgeExpiredLocked drops entries whose expiry has passed and restores the
// earliest-expiry hint. It runs only when the hint says at least one entry may
// be stale.
func (c *dockerMetadataCache) purgeExpiredLocked(now time.Time) {
	for existing, entry := range c.entries {
		if !now.Before(entry.expiry) {
			delete(c.entries, existing)
		}
	}
	c.recomputeNextExpiryLocked()
}

// evictEarliestExpiryLocked keeps a just-stored hot answer from being evicted
// at capacity: with one TTL per cache, the earliest expiry is the oldest
// insertion, which is the least likely answer a caller needs next.
func (c *dockerMetadataCache) evictEarliestExpiryLocked() {
	var oldestKey string
	var oldestExpiry time.Time
	for existing, entry := range c.entries {
		if oldestKey == "" || entry.expiry.Before(oldestExpiry) {
			oldestKey, oldestExpiry = existing, entry.expiry
		}
	}
	if oldestKey == "" {
		return
	}
	delete(c.entries, oldestKey)
	c.recomputeNextExpiryLocked()
}

func (c *dockerMetadataCache) recomputeNextExpiryLocked() {
	c.nextExpiry = time.Time{}
	for _, entry := range c.entries {
		if c.nextExpiry.IsZero() || entry.expiry.Before(c.nextExpiry) {
			c.nextExpiry = entry.expiry
		}
	}
}

// dockerTargetKey identifies one daemon: the backend, the resolved target and
// client settings, and the executable that issued the query. The effective
// query timeout changes how long a lookup may take, not what a successful
// answer means, so cache entries omit it; flights append the timeout
// separately to isolate callers with different budgets.
func dockerTargetKey(eng engine, runner cli.Runner) string {
	return eng.name() + "\x00" + eng.imageStoreID() + "\x00" + dockerRunnerKey(runner)
}

// dockerDefaultNetworkKey identifies one daemon default-network answer.
func dockerDefaultNetworkKey(eng engine, runner cli.Runner) string {
	return dockerTargetKey(eng, runner)
}

// dockerVersionJSONKey identifies the raw `docker version --format
// '{{json .}}'` answer shared by the default-network and copy-out probes, so
// one CLI invocation per target serves both.
func dockerVersionJSONKey(eng engine, runner cli.Runner) string {
	return "docker-version\x00" + dockerTargetKey(eng, runner)
}

// dockerVersionJSON runs one `docker version --format '{{json .}}'` query.
// The raw output is cached and parsed by both metadata users instead of each
// running its own version query.
func dockerVersionJSON(ctx context.Context, runner cli.Runner, timeouts opTimeouts) (string, error) {
	qTimeout := timeouts.query
	if qTimeout <= 0 {
		qTimeout = queryTimeout
	}
	qCtx, cancel := context.WithTimeout(ctx, qTimeout)
	defer cancel()
	stdout, _, err := runner.Run(qCtx, "version", "--format", "{{json .}}")
	if err != nil {
		if ctxErr := qCtx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return "", errors.Join(err, ctxErr)
		}
		return "", err
	}
	if ctxErr := qCtx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return string(stdout), nil
}

// dockerVersionJSONCached shares one raw version response through the
// optional Client cache. Only a response carrying the metadata both consumers
// need (a decodable server OS) is stored, so a successful but unusable query
// is not reused. A caller that rejects the response for another reason (for
// example an unsupported client version) deletes it so the next call retries
// instead of serving the failure until the TTL.
func dockerVersionJSONCached(ctx context.Context, cache *dockerMetadataCache, runner cli.Runner, eng engine, timeouts opTimeouts) (string, error) {
	fetch := func(ctx context.Context) (string, bool, error) {
		raw, err := dockerVersionJSON(ctx, runner, timeouts)
		if err != nil {
			return "", false, err
		}
		return raw, dockerVersionResponseUsable(raw), nil
	}
	if cache == nil {
		raw, _, err := fetch(ctx)
		return raw, err
	}
	return cache.lookupWithStore(ctx, dockerVersionJSONKey(eng, runner), timeouts.query, fetch)
}

// dockerVersionResponseUsable reports whether raw carries the metadata the
// shared consumers need: a decodable version response naming a server OS the
// library understands. Version policy is left to the copy-out check, which
// drops the entry when the reported versions are rejected.
func dockerVersionResponseUsable(raw string) bool {
	var output dockerVersionOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(output.Server.Os)) {
	case "linux", "windows":
		return true
	default:
		return false
	}
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

// dockerDefaultNetworkCached resolves the daemon default network from the
// shared version response. A nil cache performs the lookup directly.
func dockerDefaultNetworkCached(ctx context.Context, cache *dockerMetadataCache, runner cli.Runner, eng engine, timeouts opTimeouts) (string, error) {
	raw, err := dockerVersionJSONCached(ctx, cache, runner, eng, timeouts)
	if err != nil {
		return "", dockerDefaultNetworkQueryError(ctx, runner, eng, timeouts, err)
	}
	return dockerDefaultNetworkFromJSON(raw)
}

// dockerDefaultNetworkQueryError classifies a failed version query the way
// the default-network probe reports it.
func dockerDefaultNetworkQueryError(ctx context.Context, runner cli.Runner, eng engine, timeouts opTimeouts, err error) error {
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return fmt.Errorf("inspect Docker server platform: %w", contextErr)
	}
	classified := classifyErrorWithProbe(ctx, runner, err, eng, timeouts.probe)
	return fmt.Errorf("inspect Docker server platform: %w", classified)
}

// dockerDefaultNetworkFromJSON derives the daemon's default network from one
// version response. The client OS is not authoritative for a remote daemon,
// and the name "bridge" or "nat" alone does not prove that a user-defined
// network is the daemon default.
func dockerDefaultNetworkFromJSON(raw string) (string, error) {
	var output dockerVersionOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return "", fmt.Errorf("inspect Docker server platform: decode Docker version output: %w", err)
	}
	serverOS := strings.ToLower(strings.TrimSpace(output.Server.Os))
	switch serverOS {
	case "linux":
		return dockerNetworkBridge, nil
	case "windows":
		return dockerNetworkNAT, nil
	default:
		return "", fmt.Errorf("%w: unsupported Docker server OS %q", ErrNetworkMismatch, serverOS)
	}
}

// dockerCopyVersionError classifies a failed version query the way the
// copy-out capability check reports it.
func dockerCopyVersionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: Docker version query timed out: %w", ErrCopyFileFromContainerUnsupported, err)
	}
	return fmt.Errorf("%w: cannot verify Docker client/server version: %w", ErrCopyFileFromContainerUnsupported, err)
}

// dockerCopyFileFromContainerVersionCached verifies Docker's copy-out
// client/server versions from the shared version response. Only a successful
// query is stored, so a failed probe never poisons the next caller. A nil
// cache performs the check directly.
func dockerCopyFileFromContainerVersionCached(ctx context.Context, cache *dockerMetadataCache, runner cli.Runner, eng engine, timeouts opTimeouts) error {
	raw, err := dockerVersionJSONCached(ctx, cache, runner, eng, timeouts)
	if err != nil {
		return dockerCopyVersionError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkDockerCopyVersion(raw); err != nil {
		// The daemon answer is cached for the default-network probe; do not
		// let a version policy rejection become sticky for copy-out callers.
		cache.delete(dockerVersionJSONKey(eng, runner))
		return err
	}
	return nil
}
