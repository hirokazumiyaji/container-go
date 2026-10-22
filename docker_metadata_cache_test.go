package container

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// metadataRunner counts Docker metadata queries and can gate or fail them so
// tests can observe sharing, target isolation, and retry behavior.
type metadataRunner struct {
	*cpRunner

	mu             sync.Mutex
	versionCalls   int
	serverOSCalls  int
	serverOS       string
	failServerOS   bool
	failCopyVer    bool
	versionStarted chan struct{}
	versionRelease chan struct{}
	startedOnce    sync.Once
}

func newMetadataRunner() *metadataRunner {
	return &metadataRunner{
		cpRunner: &cpRunner{fakeRunner: newTestRunner(), fileContent: "metadata payload"},
	}
}

func (r *metadataRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "version" && args[1] == "--format" {
		r.mu.Lock()
		r.versionCalls++
		if args[2] == "{{.Server.Os}}" {
			r.serverOSCalls++
		}
		started := r.versionStarted
		release := r.versionRelease
		serverOS := r.serverOS
		failServerOS := r.failServerOS
		failCopyVer := r.failCopyVer
		r.mu.Unlock()

		if started != nil {
			r.startedOnce.Do(func() { close(started) })
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if args[2] == "{{.Server.Os}}" {
			if failServerOS {
				return nil, nil, &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "injected server OS failure"}
			}
			if serverOS == "" {
				serverOS = "linux"
			}
			return []byte(serverOS + "\n"), nil, nil
		}
		if failCopyVer {
			return nil, nil, &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "injected copy version failure"}
		}
		return []byte(defaultTestDockerVersion), nil, nil
	}
	return r.cpRunner.Run(ctx, args...)
}

func (r *metadataRunner) versionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.versionCalls
}

func (r *metadataRunner) serverOSCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serverOSCalls
}

func (r *metadataRunner) setFailServerOS(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failServerOS = fail
}

func dockerMetadataConfig(cache *dockerMetadataCache, runner cli.Runner, host string) *config {
	return &config{
		eng:          newDockerEngine(host, "", "", ""),
		runner:       runner,
		dockerMeta:   cache,
		queryTimeout: time.Second,
	}
}

func TestDockerDefaultNetworkCacheReusesSuccessfulLookup(t *testing.T) {
	runner := newMetadataRunner()
	cache := newDockerMetadataCache(time.Minute)
	cfg := dockerMetadataConfig(cache, runner, "tcp://daemon-a:2375")

	for i := 0; i < 2; i++ {
		info := &engineInfo{}
		if err := attachDefaultNetworkForConfig(context.Background(), cfg, info); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		if info.defaultNetwork != dockerNetworkBridge {
			t.Fatalf("attach %d default network = %q, want %q", i, info.defaultNetwork, dockerNetworkBridge)
		}
	}
	if got := runner.serverOSCount(); got != 1 {
		t.Fatalf("server OS lookups = %d, want 1", got)
	}
}

func TestDockerDefaultNetworkCacheSharesConcurrentLookup(t *testing.T) {
	runner := newMetadataRunner()
	runner.versionStarted = make(chan struct{})
	runner.versionRelease = make(chan struct{})
	cache := newDockerMetadataCache(time.Minute)
	joined := make(chan struct{})
	var joinedOnce sync.Once
	cache.flights.onJoin = func(string) {
		joinedOnce.Do(func() { close(joined) })
	}
	cfg := dockerMetadataConfig(cache, runner, "tcp://daemon-a:2375")

	start := make(chan struct{})
	results := make(chan *engineInfo, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			info := &engineInfo{}
			if err := attachDefaultNetworkForConfig(context.Background(), cfg, info); err != nil {
				errs <- err
				return
			}
			results <- info
		}()
	}
	close(start)
	<-runner.versionStarted
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("second caller never joined the in-flight default-network lookup")
	}
	close(runner.versionRelease)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			t.Fatalf("attach: %v", err)
		case info := <-results:
			if info.defaultNetwork != dockerNetworkBridge {
				t.Fatalf("default network = %q, want %q", info.defaultNetwork, dockerNetworkBridge)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for attach")
		}
	}
	if got := runner.serverOSCount(); got != 1 {
		t.Fatalf("server OS lookups = %d, want 1", got)
	}
}

func TestDockerMetadataCacheLeaderCancellationDoesNotFailJoiners(t *testing.T) {
	cache := newDockerMetadataCache(time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func(ctx context.Context) (string, error) {
		close(started)
		<-release
		return dockerNetworkBridge, ctx.Err()
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := cache.lookup(leaderCtx, "shared", fn)
		leaderErr <- err
	}()
	<-started
	cancelLeader()

	joinerDone := make(chan struct{})
	var got string
	var gotErr error
	go func() {
		got, gotErr = cache.lookup(context.Background(), "shared", fn)
		close(joinerDone)
	}()
	close(release)
	<-joinerDone

	if gotErr != nil {
		t.Fatalf("joiner error = %v, want nil", gotErr)
	}
	if got != dockerNetworkBridge {
		t.Fatalf("joiner value = %q, want %q", got, dockerNetworkBridge)
	}
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want its own context.Canceled", err)
	}
}

func TestDockerDefaultNetworkCacheSeparatesTargets(t *testing.T) {
	cache := newDockerMetadataCache(time.Minute)
	linux := newMetadataRunner()
	windows := newMetadataRunner()
	windows.serverOS = "windows"

	infoA := &engineInfo{}
	if err := attachDefaultNetworkForConfig(context.Background(), dockerMetadataConfig(cache, linux, "tcp://daemon-a:2375"), infoA); err != nil {
		t.Fatalf("attach daemon-a: %v", err)
	}
	infoB := &engineInfo{}
	if err := attachDefaultNetworkForConfig(context.Background(), dockerMetadataConfig(cache, windows, "tcp://daemon-b:2375"), infoB); err != nil {
		t.Fatalf("attach daemon-b: %v", err)
	}
	if infoA.defaultNetwork != dockerNetworkBridge || infoB.defaultNetwork != dockerNetworkNAT {
		t.Fatalf("default networks = %q/%q, want %q/%q", infoA.defaultNetwork, infoB.defaultNetwork, dockerNetworkBridge, dockerNetworkNAT)
	}
	if got := linux.serverOSCount(); got != 1 {
		t.Fatalf("daemon-a lookups = %d, want 1", got)
	}
	if got := windows.serverOSCount(); got != 1 {
		t.Fatalf("daemon-b lookups = %d, want 1", got)
	}
}

func TestDockerDefaultNetworkCacheRetriesFailedLookup(t *testing.T) {
	runner := newMetadataRunner()
	runner.failServerOS = true
	cache := newDockerMetadataCache(time.Minute)
	cfg := dockerMetadataConfig(cache, runner, "tcp://daemon-a:2375")

	info := &engineInfo{}
	if err := attachDefaultNetworkForConfig(context.Background(), cfg, info); err == nil {
		t.Fatal("first attach succeeded, want the injected failure")
	}

	runner.setFailServerOS(false)
	info = &engineInfo{}
	if err := attachDefaultNetworkForConfig(context.Background(), cfg, info); err != nil {
		t.Fatalf("retry attach: %v", err)
	}
	if info.defaultNetwork != dockerNetworkBridge {
		t.Fatalf("default network = %q, want %q", info.defaultNetwork, dockerNetworkBridge)
	}
	if got := runner.serverOSCount(); got != 2 {
		t.Fatalf("server OS lookups = %d, want 2 (failure then retry)", got)
	}
}

func TestDockerDefaultNetworkCacheExpires(t *testing.T) {
	runner := newMetadataRunner()
	cache := newDockerMetadataCache(time.Minute)
	now := time.Unix(0, 0)
	cache.now = func() time.Time { return now }
	cfg := dockerMetadataConfig(cache, runner, "tcp://daemon-a:2375")

	for i := 0; i < 2; i++ {
		if err := attachDefaultNetworkForConfig(context.Background(), cfg, &engineInfo{}); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
	}
	now = now.Add(time.Minute)
	if err := attachDefaultNetworkForConfig(context.Background(), cfg, &engineInfo{}); err != nil {
		t.Fatalf("attach after expiry: %v", err)
	}
	if got := runner.serverOSCount(); got != 2 {
		t.Fatalf("server OS lookups = %d, want 2", got)
	}
}

func TestClientSharesDefaultNetworkLookupAcrossHandles(t *testing.T) {
	inspectJSON, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerRunner{fakeRunner: newTestRunner(), inspectJSON: inspectJSON}
	client := &Client{
		eng:        newDockerEngine("tcp://daemon-a:2375", "", "", ""),
		runner:     runner,
		dockerMeta: newDockerMetadataCache(time.Minute),
	}

	for _, name := range []string{"first", "second"} {
		ctr, err := client.Run(context.Background(), "redis:7-alpine", WithName(name))
		if err != nil {
			t.Fatalf("Run %s: %v", name, err)
		}
		if ctr.dockerMeta != client.dockerMeta {
			t.Fatalf("handle %s did not inherit the Client metadata cache", name)
		}
		if _, err := ctr.ensureDefaultNetwork(context.Background()); err != nil {
			t.Fatalf("ensure default network %s: %v", name, err)
		}
	}

	lookups := 0
	for _, args := range runner.calls {
		if len(args) >= 3 && args[0] == "version" && args[2] == "{{.Server.Os}}" {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("server OS lookups = %d, want 1 across the Client's handles", lookups)
	}
}

func TestContainerDefaultNetworkLookupSharedByConcurrentCallers(t *testing.T) {
	runner := newMetadataRunner()
	runner.versionStarted = make(chan struct{})
	runner.versionRelease = make(chan struct{})
	eng := newDockerEngine("tcp://daemon-a:2375", "", "", "")
	base := &Container{id: "myctr", runner: runner, eng: eng, sync: &lockPack{}}
	// No Client cache: the shared ensure result itself must collapse the
	// concurrent reuse attaches on this target.
	cfg := dockerMetadataConfig(nil, runner, "tcp://daemon-a:2375")

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := base.ensureDefaultNetworkForConfig(context.Background(), cfg)
			errs <- err
		}()
	}
	close(start)
	<-runner.versionStarted
	close(runner.versionRelease)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent attach: %v", err)
		}
	}
	if got := runner.serverOSCount(); got != 1 {
		t.Fatalf("server OS lookups = %d, want 1", got)
	}
}

func TestCopyFileFromContainerCachesVersionCheckPerHandle(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	runner := newMetadataRunner()
	ctr := runCopyDockerTestContainer(t, runner)

	for i := 0; i < 2; i++ {
		rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("read copy %d: %v", i, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("close copy %d: %v", i, err)
		}
	}
	if got := runner.versionCount(); got != 1 {
		t.Fatalf("copy version lookups = %d, want 1", got)
	}
}

func TestCopyFileFromContainerSharesVersionCheckConcurrently(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	runner := newMetadataRunner()
	runner.versionStarted = make(chan struct{})
	runner.versionRelease = make(chan struct{})
	ctr := runCopyDockerTestContainer(t, runner)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
			if err == nil {
				_, err = io.ReadAll(rc)
				if closeErr := rc.Close(); err == nil {
					err = closeErr
				}
			}
			errs <- err
		}()
	}
	close(start)
	<-runner.versionStarted
	close(runner.versionRelease)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent copy: %v", err)
		}
	}
	if got := runner.versionCount(); got != 1 {
		t.Fatalf("copy version lookups = %d, want 1", got)
	}
}

func TestCopyFileFromContainerVersionCheckIsPerHandle(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	first := newMetadataRunner()
	second := newMetadataRunner()
	for i, runner := range []*metadataRunner{first, second} {
		ctr := runCopyDockerTestContainer(t, runner)
		rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
		if err != nil {
			t.Fatalf("handle %d copy: %v", i, err)
		}
		_ = rc.Close()
	}
	if got := first.versionCount(); got != 1 {
		t.Fatalf("first handle lookups = %d, want 1", got)
	}
	if got := second.versionCount(); got != 1 {
		t.Fatalf("second handle lookups = %d, want 1", got)
	}
}

func TestCopyFileFromContainerRetriesFailedVersionCheck(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	runner := newMetadataRunner()
	runner.failCopyVer = true
	ctr := runCopyDockerTestContainer(t, runner)

	if _, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt"); !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("first copy error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}

	runner.mu.Lock()
	runner.failCopyVer = false
	runner.mu.Unlock()
	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if err != nil {
		t.Fatalf("retry copy: %v", err)
	}
	_ = rc.Close()
	if got := runner.versionCount(); got != 2 {
		t.Fatalf("copy version lookups = %d, want 2 (failure then retry)", got)
	}
}
