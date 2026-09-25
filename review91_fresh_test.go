package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type review91IdentitySwitchRunner struct {
	*fakeRunner
	mu          sync.Mutex
	inspects    int
	first       chan struct{}
	releaseBase chan struct{}
}

func (r *review91IdentitySwitchRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}

	r.mu.Lock()
	r.inspects++
	inspect := r.inspects
	r.mu.Unlock()

	switch inspect {
	case 1:
		close(r.first)
		created := strings.Replace(review91ReuseInspectJSON("uid-a", "generation-a", false), `"Status":"running"`, `"Status":"created"`, 1)
		return []byte(created), nil, nil
	case 2:
		<-r.releaseBase
		return []byte(review91ReuseInspectJSON("uid-a", "generation-a", false)), nil, nil
	default:
		return []byte(review91ReuseInspectJSON("uid-b", "generation-b", true)), nil, nil
	}
}

func review91ReuseInspectJSON(uid, generation string, bound bool) string {
	ports := `{}`
	if bound {
		ports = `{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}`
	}
	return fmt.Sprintf(`[{
		"Id":%q,
		"Name":"/shared",
		"Config":{"Image":"redis:7-alpine","Labels":{
			"com.github.hirokazumiyaji.container-go.reuse":"true",
			"com.github.hirokazumiyaji.container-go.creation":%q
		}},
		"State":{"Status":"running"},
		"NetworkSettings":{"Ports":%s}
	}]`, uid, generation, ports)
}

func TestReview91ReuseCallerRefreshRejectsReplacedIdentity(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = time.Second
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	runner := &review91IdentitySwitchRunner{
		fakeRunner:  newTestRunner(),
		first:       make(chan struct{}),
		releaseBase: make(chan struct{}),
	}
	runner.imagePresent = true

	joined := make(chan struct{})
	var joinedOnce sync.Once
	oldHook := reuseFlights.onJoin
	reuseFlights.onJoin = func(string) { joinedOnce.Do(func() { close(joined) }) }
	t.Cleanup(func() { reuseFlights.onJoin = oldHook })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	leaderResult := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "redis:7-alpine",
			WithName("shared"), WithReuse(),
			withRunner(runner), withEngine(dockerEngine{}))
		leaderResult <- err
	}()

	<-runner.first
	waiterResult := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "redis:7-alpine",
			WithName("shared"), WithReuse(), WithExposedPorts("6379/tcp"),
			withRunner(runner), withEngine(dockerEngine{}))
		waiterResult <- err
	}()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("second caller did not join the reuse flight")
	}
	close(runner.releaseBase)

	if err := <-leaderResult; !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("leader error = %v, want ErrGenerationReplaced", err)
	}
	if err := <-waiterResult; !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("waiting caller error = %v, want ErrGenerationReplaced", err)
	}
}

type review91SingleInspectRunner struct {
	data string
	err  error
}

func (r review91SingleInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(r.data), nil, r.err
	}
	return nil, nil, nil
}

func TestReview91ReuseRefreshVerifiesGenerationAndUID(t *testing.T) {
	cases := map[string]struct {
		creation string
		uid      string
	}{
		"generation": {creation: "generation-b", uid: "uid-a"},
		"uid":        {creation: "generation-a", uid: "uid-b"},
	}
	for name, replacement := range cases {
		t.Run(name, func(t *testing.T) {
			initial := &engineInfo{
				state:  StateCreated,
				labels: map[string]string{creationLabel: "generation-a"},
				uid:    "uid-a",
			}
			base := &Container{
				creation: "generation-a",
				uid:      "uid-a",
				info:     initial,
			}
			cfg := &config{
				runner: review91SingleInspectRunner{data: review91ReuseInspectJSON(replacement.uid, replacement.creation, false)},
				eng:    dockerEngine{},
				name:   "shared",
			}
			info, err := reuseInfoForCaller(context.Background(), cfg, base)
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("info = %+v, error = %v, want ErrGenerationReplaced", info, err)
			}
			if info != nil {
				t.Fatalf("info = %+v, want no mixed identity", info)
			}
		})
	}
}

type review91CreatedRunner struct{}

func (review91CreatedRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(`[{
			"Id":"uid-a",
			"Config":{"Image":"redis:7-alpine","Labels":{
				"com.github.hirokazumiyaji.container-go.reuse":"true",
				"com.github.hirokazumiyaji.container-go.creation":"generation-a"
			}},
			"State":{"Status":"created"},
			"NetworkSettings":{}
		}]`), nil, nil
	}
	return nil, nil, nil
}

func TestReview91ReuseEnsureTimeoutPreservesDeadlineExceeded(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := reuseEnsureContainer(ctx, "redis:7-alpine", &config{
		runner: review91CreatedRunner{},
		eng:    dockerEngine{},
		name:   "shared",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

type review91RefreshTimeoutRunner struct {
	*fakeRunner
	mu          sync.Mutex
	inspects    int
	first       chan struct{}
	releaseBase chan struct{}
}

func (r *review91RefreshTimeoutRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.inspects++
	inspect := r.inspects
	r.mu.Unlock()
	switch inspect {
	case 1:
		close(r.first)
		created := strings.Replace(review91ReuseInspectJSON("uid-a", "generation-a", false), `"Status":"running"`, `"Status":"created"`, 1)
		return []byte(created), nil, nil
	case 2:
		<-r.releaseBase
		return []byte(review91ReuseInspectJSON("uid-a", "generation-a", false)), nil, nil
	default:
		return nil, nil, errors.New("temporary refreshed inspect failure")
	}
}

func TestReview91ReuseCallerRefreshTimeoutPreservesDeadlineExceeded(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	runner := &review91RefreshTimeoutRunner{
		fakeRunner:  newTestRunner(),
		first:       make(chan struct{}),
		releaseBase: make(chan struct{}),
	}
	runner.imagePresent = true
	joined := make(chan struct{})
	var joinedOnce sync.Once
	oldHook := reuseFlights.onJoin
	reuseFlights.onJoin = func(string) { joinedOnce.Do(func() { close(joined) }) }
	t.Cleanup(func() { reuseFlights.onJoin = oldHook })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	leaderResult := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "redis:7-alpine",
			WithName("shared"), WithReuse(),
			withRunner(runner), withEngine(dockerEngine{}))
		leaderResult <- err
	}()

	<-runner.first
	waiterResult := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "redis:7-alpine",
			WithName("shared"), WithReuse(), WithExposedPorts("6379/tcp"),
			withRunner(runner), withEngine(dockerEngine{}))
		waiterResult <- err
	}()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("second caller did not join the reuse flight")
	}
	close(runner.releaseBase)

	if err := <-leaderResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("leader error = %v, want context.DeadlineExceeded", err)
	}
	if err := <-waiterResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller error = %v, want context.DeadlineExceeded", err)
	}
}

type review91LazyUIDRunner struct {
	inspectDelay time.Duration
	mu           sync.Mutex
	deleted      []string
}

func (r *review91LazyUIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		// Keep inspect in flight while concurrent Terminate calls read the
		// lazy immutable-ID field.
		time.Sleep(r.inspectDelay)
		return []byte(review91OwnedDockerInspect("immutable-uid", "generation-a", false, false)), nil, nil
	case "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestReview91LazyUIDCacheConcurrentWithTerminate(t *testing.T) {
	runner := &review91LazyUIDRunner{inspectDelay: 50 * time.Millisecond}
	ctr := &Container{
		id:       "shared",
		runner:   runner,
		eng:      dockerEngine{},
		creation: "generation-a",
	}

	start := make(chan struct{})
	cacheResult := make(chan error, 1)
	go func() {
		<-start
		_, err := ctr.cachedInfo(context.Background())
		cacheResult <- err
	}()

	const terminators = 8
	terminateResults := make(chan error, terminators)
	for range terminators {
		go func() {
			<-start
			terminateResults <- ctr.Terminate(context.Background())
		}()
	}
	close(start)

	if err := <-cacheResult; err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
	for range terminators {
		if err := <-terminateResults; err != nil {
			t.Fatalf("Terminate: %v", err)
		}
	}
	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != terminators {
		t.Fatalf("delete calls = %d, want %d", len(deleted), terminators)
	}
	for _, target := range deleted {
		if target != "immutable-uid" {
			t.Fatalf("deleted target = %q, want immutable UID", target)
		}
	}
}

func TestReview91ReuseContextErrorWrapsDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := reuseContextError(ctx, "shared")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "shared") {
		t.Fatalf("error = %v, want container name", err)
	}
}
