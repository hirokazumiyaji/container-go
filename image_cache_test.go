package container

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// countInspects reports how many recorded CLI invocations are
// `<subcommand> inspect`. It takes the recorded calls rather than a
// runner so the wrapper runners below can be counted too.
func countInspects(calls [][]string, subcommand string) int {
	n := 0
	for _, c := range calls {
		if len(c) >= 2 && c[0] == subcommand && c[1] == "inspect" {
			n++
		}
	}
	return n
}

// TestImageCacheDisabledByDefault pins that the default behavior is
// unchanged: Run still inspects on every call, so a caller who needs
// certainty about the daemon's store is not silently given a stale
// answer.
func TestImageCacheDisabledByDefault(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}

	ctx := context.Background()
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects for 2 Runs with no cache, want 2", got)
	}
}

// TestImageCacheSkipsTheSecondInspect is the point of the option:
// reusing one Option across sequential configs (as Run does) skips the
// second inspect.
func TestImageCacheSkipsTheSecondInspect(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	opt := WithImagePresenceCache(time.Minute)

	cfg1 := newConfig()
	cfg1.runner = r
	cfg1.eng = dockerEngine{}
	if err := opt(cfg1); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := cfg1.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 1 {
		t.Fatalf("first Run ran %d image inspects, want 1", got)
	}

	cfg2 := newConfig()
	cfg2.runner = r
	cfg2.eng = dockerEngine{}
	if err := opt(cfg2); err != nil {
		t.Fatal(err)
	}
	if err := cfg2.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 1 {
		t.Errorf("second Run ran an extra image inspect (total %d, want 1)", got)
	}
}

// TestImageCacheIsNotSharedAcrossDistinctOptions keeps a fresh
// WithImagePresenceCache call from observing another caller's entries.
func TestImageCacheIsNotSharedAcrossDistinctOptions(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	ctx := context.Background()

	cfg1 := newConfig()
	cfg1.runner = r
	cfg1.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg1); err != nil {
		t.Fatal(err)
	}
	if err := cfg1.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}

	cfg2 := newConfig()
	cfg2.runner = r
	cfg2.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg2); err != nil {
		t.Fatal(err)
	}
	if err := cfg2.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects across distinct Options, want 2", got)
	}
}

// TestImageCacheExpires covers the TTL: an entry past its lifetime must
// stop answering, so a cache cannot hide an image removed out of band
// forever.
func TestImageCacheExpires(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}
	// Drive the cache's clock rather than sleeping.
	now := time.Now()
	cfg.imageCache.now = func() time.Time { return now }

	ctx := context.Background()
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	// Just inside the window: still cached.
	now = now.Add(59 * time.Second)
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("ensureImage inside the window: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 1 {
		t.Errorf("ran %d inspects inside the cache window, want 1", got)
	}
	// Past the window: expired, so the daemon is asked again.
	now = now.Add(2 * time.Second)
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("ensureImage after expiry: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d inspects after the entry expired, want 2", got)
	}
}

// TestImageCacheForgetsExpiredEntries keeps the map from growing without
// bound when an image is looked up repeatedly past its TTL.
func TestImageCacheForgetsExpiredEntries(t *testing.T) {
	c := newImageCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }

	c.remember(dockerEngine{}, "redis:7-alpine", "")
	if got := c.size(); got != 1 {
		t.Fatalf("size = %d after remember, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	if c.seen(dockerEngine{}, "redis:7-alpine", "") {
		t.Error("seen() reported a fresh entry for an expired one")
	}
	if got := c.size(); got != 0 {
		t.Errorf("size = %d after an expired entry was observed, want 0 (it should be dropped)", got)
	}
}

// TestImageCacheRememberPurgesExpiredEntries covers keys that expire
// without ever being looked up again: remember of a different image must
// still drop them so a long-lived Option does not retain dead entries.
func TestImageCacheRememberPurgesExpiredEntries(t *testing.T) {
	c := newImageCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }

	c.remember(dockerEngine{}, "old:1", "")
	now = now.Add(2 * time.Minute)
	c.remember(dockerEngine{}, "new:1", "")
	if got := c.size(); got != 1 {
		t.Errorf("size = %d after remember purged expired, want 1", got)
	}
	if c.seen(dockerEngine{}, "old:1", "") {
		t.Error("expired entry survived a remember of a different key")
	}
	if !c.seen(dockerEngine{}, "new:1", "") {
		t.Error("fresh entry missing after remember")
	}
}

// TestImageCacheWaitersPopulateOwnCaches covers concurrent PullMissing
// Runs that share a flight but hold distinct Options: only the leader
// used to record presence, so a later Run reusing a waiter's Option
// paid another inspect. Every successful waiter must populate its cache.
func TestImageCacheWaitersPopulateOwnCaches(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true

	inspectEntered := make(chan struct{})
	releaseInspect := make(chan struct{})
	var inspectOnce sync.Once
	r := &hookRunner{
		fakeRunner: base,
		before: func(args []string) {
			if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				inspectOnce.Do(func() { close(inspectEntered) })
				<-releaseInspect
			}
		},
	}

	optLeader := WithImagePresenceCache(time.Minute)
	optWaiter := WithImagePresenceCache(time.Minute)

	leaderDone := make(chan error, 1)
	go func() {
		cfg := newConfig()
		cfg.runner = r
		cfg.eng = dockerEngine{}
		if err := optLeader(cfg); err != nil {
			leaderDone <- err
			return
		}
		leaderDone <- cfg.ensureImage(context.Background(), "redis:7-alpine")
	}()
	select {
	case <-inspectEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never entered image inspect")
	}

	waiterDone := make(chan error, 1)
	go func() {
		cfg := newConfig()
		cfg.runner = r
		cfg.eng = dockerEngine{}
		if err := optWaiter(cfg); err != nil {
			waiterDone <- err
			return
		}
		waiterDone <- cfg.ensureImage(context.Background(), "redis:7-alpine")
	}()
	// Give the waiter time to join the in-flight inspect.
	time.Sleep(50 * time.Millisecond)
	close(releaseInspect)

	if err := <-leaderDone; err != nil {
		t.Fatalf("leader: %v", err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter: %v", err)
	}

	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := optWaiter(cfg); err != nil {
		t.Fatal(err)
	}
	before := countInspects(base.calls, "image")
	if err := cfg.ensureImage(context.Background(), "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(base.calls, "image"); got != before {
		t.Errorf("waiter Option cache missed: inspects grew from %d to %d", before, got)
	}
}

// TestImageCacheDoesNotCacheAbsence is the safety property: a missing
// image is pulled, and the cache records the presence the pull created
// rather than the absence that triggered it. Caching the absence would
// mean a second Run skipped the image that the first Run had just
// fetched.
func TestImageCacheDoesNotCacheAbsence(t *testing.T) {
	r := newTestRunner()
	// The image is absent: image inspect fails, then the pull succeeds.
	r.imagePresent = false
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	if r.pullCalls != 1 {
		t.Fatalf("pullCalls = %d, want 1", r.pullCalls)
	}
	// The pull made the image present, so the second Run must not
	// inspect and must not pull again.
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 1 {
		t.Errorf("ran %d image inspects across 2 Runs, want 1", got)
	}
	if r.pullCalls != 1 {
		t.Errorf("pullCalls = %d after the image was cached present, want 1", r.pullCalls)
	}
}

// failingPullRunner answers inspect with "missing" and rejects the pull.
// fakeRunner.failPrefix cannot express this: its pull branch runs before
// the failPrefix check.
type failingPullRunner struct {
	*fakeRunner
}

func (r *failingPullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	// Both spellings: docker pulls with `docker pull`, Apple with
	// `container images pull`.
	if (len(args) >= 2 && args[0] == "image" && args[1] == "pull") || args[0] == "pull" {
		r.calls = append(r.calls, args)
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected pull failure"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

// TestImageCacheIsNotWrittenWhenThePullFails keeps a failed pull from
// recording a presence that does not exist.
func TestImageCacheIsNotWrittenWhenThePullFails(t *testing.T) {
	r := &failingPullRunner{fakeRunner: newTestRunner()}
	r.imagePresent = false
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err == nil {
		t.Fatal("want an error when the pull fails")
	}
	if got := cfg.imageCache.size(); got != 0 {
		t.Errorf("cache holds %d entries after a failed pull, want 0", got)
	}
	// And the next call must inspect and try again rather than trusting
	// the cache.
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err == nil {
		t.Fatal("want an error again on the retry")
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects across the failed attempts, want 2", got)
	}
}

// TestImageCacheIsPerDockerHost keeps a presence recorded against one
// DOCKER_HOST from answering for another: the two daemons are independent
// stores, and PullMissing would otherwise skip the inspect and fail with
// --pull=never on the second host.
func TestImageCacheIsPerDockerHost(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	opt := WithImagePresenceCache(time.Minute)
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := opt(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects across DOCKER_HOST values, want 2", got)
	}
}

// TestImageCacheIsPerDockerContext covers the common case where
// DOCKER_HOST is empty and DOCKER_CONTEXT selects the daemon.
func TestImageCacheIsPerDockerContext(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	opt := WithImagePresenceCache(time.Minute)
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := opt(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "desktop-linux")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "remote-ci")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects across DOCKER_CONTEXT values, want 2", got)
	}
}

// TestImageCacheIsPerDockerConfig covers two profiles that share a
// context name but keep different context definitions under DOCKER_CONFIG.
func TestImageCacheIsPerDockerConfig(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	opt := WithImagePresenceCache(time.Minute)
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := opt(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "ci")
	t.Setenv("DOCKER_CONFIG", "/tmp/docker-config-a")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", "/tmp/docker-config-b")
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects across DOCKER_CONFIG values, want 2", got)
	}
}

// TestImageCacheIsPerPlatformAndPerBackend keeps entries from being
// shared where the underlying stores are independent.
func TestImageCacheIsPerPlatformAndPerBackend(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	opt := WithImagePresenceCache(time.Minute)
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := opt(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	// A different platform is a different question: the cache must not
	// answer it from the default-platform entry.
	cfg.platform = "linux/arm64"
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects for two platforms, want 2", got)
	}
	// And the other backend's store is independent too — same Option, so
	// the miss comes from the key, not from a fresh empty cache.
	appleCfg := newConfig()
	appleCfg.runner = r
	appleCfg.eng = appleEngine{}
	if err := opt(appleCfg); err != nil {
		t.Fatal(err)
	}
	if err := appleCfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if got := countInspects(r.calls, "image"); got != 3 {
		t.Errorf("ran %d image inspects across two backends, want 3", got)
	}
}

// TestPullNeverStillInspects keeps the cache out of the PullNever path.
// PullNever's contract is that Run fails when the image is absent, so
// answering from a cache would turn a definite failure into a stale
// success.
func TestPullNeverStillInspects(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithPullPolicy(PullNever)(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if got := countInspects(r.calls, "image"); got != 2 {
		t.Errorf("ran %d image inspects under PullNever, want 2 (the cache must not apply)", got)
	}
}

// TestPullAlwaysStillPullsAndRecords covers the PullAlways interaction:
// the mandatory pull happens regardless of the cache, and its success is
// recorded.
func TestPullAlwaysStillPullsAndRecords(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = dockerEngine{}
	if err := WithImagePresenceCache(time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithPullPolicy(PullAlways)(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	if err := cfg.ensureImage(ctx, "redis:7-alpine"); err != nil {
		t.Fatalf("second ensureImage: %v", err)
	}
	if r.pullCalls != 2 {
		t.Errorf("pullCalls = %d under PullAlways, want 2: the cache must not suppress a mandatory pull", r.pullCalls)
	}
	if got := countInspects(r.calls, "image"); got != 0 {
		t.Errorf("ran %d image inspects under PullAlways, want 0", got)
	}
	if got := cfg.imageCache.size(); got != 1 {
		t.Errorf("cache holds %d entries after two successful pulls, want 1", got)
	}
}

// TestWithImagePresenceCacheRejectsNegativeTTL keeps a nonsensical TTL
// from silently disabling the cache.
func TestWithImagePresenceCacheRejectsNegativeTTL(t *testing.T) {
	cfg := newConfig()
	if err := WithImagePresenceCache(-time.Second)(cfg); err == nil {
		t.Fatal("want an error for a negative ttl")
	}
	if cfg.imageCache != nil {
		t.Error("a rejected ttl installed a cache")
	}
}

// TestWithImagePresenceCacheZeroIsDisabled makes the "same as not
// passing the option" claim in the doc checkable.
func TestWithImagePresenceCacheZeroIsDisabled(t *testing.T) {
	cfg := newConfig()
	if err := WithImagePresenceCache(0)(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.imageCache.enabled() {
		t.Error("a zero ttl produced an enabled cache")
	}
	cfg.imageCache.remember(dockerEngine{}, "redis:7-alpine", "")
	if cfg.imageCache.seen(dockerEngine{}, "redis:7-alpine", "") {
		t.Error("a disabled cache reported a hit")
	}
}

// TestNilImageCacheIsInert covers the default config, whose cache field
// is nil: every method has to tolerate that.
func TestNilImageCacheIsInert(t *testing.T) {
	var c *imageCache
	if c.enabled() {
		t.Error("nil cache reported enabled")
	}
	if c.seen(dockerEngine{}, "redis:7-alpine", "") {
		t.Error("nil cache reported a hit")
	}
	// These must not panic on a nil receiver.
	c.remember(dockerEngine{}, "redis:7-alpine", "")
	c.forget(dockerEngine{}, "redis:7-alpine", "")
}

// TestImageCacheSurvivesConcurrentRuns exercises the cache from several
// goroutines, which is what a shared test binary does.
func TestImageCacheSurvivesConcurrentRuns(t *testing.T) {
	c := newImageCache(time.Minute)
	const workers = 16
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			c.remember(dockerEngine{}, "redis:7-alpine", "")
			c.seen(dockerEngine{}, "redis:7-alpine", "")
			c.forget(dockerEngine{}, "redis:7-alpine", "")
			c.size()
		}()
	}
	for range workers {
		<-done
	}
}
