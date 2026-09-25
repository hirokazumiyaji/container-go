package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestReuseCreateUsesFreshGenerationForEveryAttempt(t *testing.T) {
	r := &generationRetryRunner{}
	cfg := &config{
		runner:        r,
		eng:           appleEngine{},
		name:          "generation-retry",
		imagePrepared: true,
	}
	if _, err := reuseCreate(context.Background(), "redis:7-alpine", cfg); err == nil {
		t.Fatal("first create attempt should return its conflict")
	}
	first := cfg.creation
	ctr, err := reuseCreate(context.Background(), "redis:7-alpine", cfg)
	if err != nil {
		t.Fatalf("second create attempt: %v", err)
	}
	if ctr == nil || ctr.creation == first || ctr.creation == "" {
		t.Fatalf("second handle = %+v, first generation = %q", ctr, first)
	}
	if len(r.creations) != 2 || r.creations[0] == r.creations[1] {
		t.Fatalf("run generations = %v, want two distinct values", r.creations)
	}
}

type generationRetryRunner struct {
	mu         sync.Mutex
	creations  []string
	generation string
}

func (r *generationRetryRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		var generation string
		for i := range args {
			if i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				generation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		r.mu.Lock()
		r.creations = append(r.creations, generation)
		r.generation = generation
		count := len(r.creations)
		r.mu.Unlock()
		if count == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "already exists"}
		}
		return []byte("generation-retry\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		generation := r.generation
		r.mu.Unlock()
		if generation == "" {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "not found"}
		}
		return reviewAppleInspect("generation-retry", "running", "redis:7-alpine", generation, "linux/amd64"), nil, nil
	case "system", "version":
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestKeepNeverReturnsStaleReuseHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	newRunner := func() *reviewDockerRunner {
		return newReviewDockerRunner(strings.Repeat("a", 64), "linux/amd64")
	}
	replacement := newRunner()
	replacement.newUID = strings.Repeat("b", 64)
	replacement.replaceOnCopy = false
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-stale"), WithReuse(), WithWaitStrategy(&reviewReplaceOnWait{replace: func() {
			replacement.mu.Lock()
			replacement.uid = replacement.newUID
			replacement.mu.Unlock()
		}}), withRunner(replacement), withEngine(dockerEngine{}))
	if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("replacement: (%v, %v), want nil handle and generation error", ctr, err)
	}

	stopped := newRunner()
	ctr, err = Run(context.Background(), "redis:7-alpine",
		WithName("keep-stopped"), WithReuse(), WithWaitStrategy(&reviewReplaceOnWait{replace: func() {
			stopped.mu.Lock()
			stopped.state = "exited"
			stopped.mu.Unlock()
		}}), withRunner(stopped), withEngine(dockerEngine{}))
	if err == nil || ctr != nil || !strings.Contains(err.Error(), "state changed") {
		t.Fatalf("stopped: (%v, %v), want nil handle and stopped error", ctr, err)
	}
}

func TestNameInspectDoesNotBindAnUnverifiedDockerUID(t *testing.T) {
	uid := strings.Repeat("e", 64)
	r := &dockerOperationRunner{uid: uid}
	ctr := &Container{id: "logical-name", runner: r, eng: dockerEngine{}, nameInspect: true}
	info, err := ctr.inspectFresh(context.Background())
	if err != nil {
		t.Fatalf("name inspect: %v", err)
	}
	if info.uid != uid || ctr.immutableID() != "" {
		t.Fatalf("name inspect bound UID: info=%q handle=%q", info.uid, ctr.immutableID())
	}
	if ctr.operationTarget() != "" {
		t.Fatalf("unbound operation target = %q, want empty", ctr.operationTarget())
	}
}

func TestDockerOperationsUseImmutableUID(t *testing.T) {
	uid := strings.Repeat("a", 64)
	r := &dockerOperationRunner{uid: uid}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("docker-ops"), WithExposedPorts("6379/tcp"),
		withRunner(r), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()

	if _, err := ctr.inspectFresh(context.Background()); err != nil {
		t.Fatalf("inspectFresh: %v", err)
	}
	if _, err := ctr.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(context.Background(), file, "/input"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if reader, err := ctr.Logs(context.Background()); err != nil {
		t.Fatalf("Logs: %v", err)
	} else {
		_ = reader.Close()
	}
	if reader, err := ctr.FollowLogs(context.Background()); err != nil {
		t.Fatalf("FollowLogs: %v", err)
	} else {
		_ = reader.Close()
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, subcommand := range []string{"inspect", "stop", "cp", "exec", "logs"} {
		found := false
		for _, call := range r.calls {
			if len(call) == 0 || call[0] != subcommand {
				continue
			}
			found = true
			if !containsTarget(call, uid) {
				t.Errorf("%s call = %v, want UID %q", subcommand, call, uid)
			}
		}
		if !found {
			t.Errorf("missing %s call in %v", subcommand, r.calls)
		}
	}
}

func containsTarget(call []string, uid string) bool {
	for _, arg := range call {
		if arg == uid || strings.HasPrefix(arg, uid+":") {
			return true
		}
	}
	return false
}

type dockerOperationRunner struct {
	mu    sync.Mutex
	uid   string
	calls [][]string
}

func (r *dockerOperationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	switch args[0] {
	case "image":
		return []byte(`[{"Id":"sha256:image"}]`), nil, nil
	case "run":
		return []byte(r.uid + "\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{}},"NetworkSettings":{"Ports":{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}}]`, r.uid)), nil, nil
	case "version", "info":
		return []byte("ok"), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *dockerOperationRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	return io.NopCloser(strings.NewReader("stream")), nil
}

func TestDockerPruneUsesIDsAndRevalidates(t *testing.T) {
	uid := strings.Repeat("b", 64)
	r := &dockerPruneRunner{uid: uid, state: "exited"}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil {
		t.Fatalf("pruneWith: %v", err)
	}
	if len(removed) != 1 || removed[0] != uid {
		t.Fatalf("removed = %v, want [%s]", removed, uid)
	}
	if len(r.deleted) != 1 || r.deleted[0] != uid {
		t.Fatalf("deleted = %v, want [%s]", r.deleted, uid)
	}
	if len(r.inspected) != 1 || r.inspected[0] != uid {
		t.Fatalf("inspected = %v, want [%s]", r.inspected, uid)
	}

	replacement := &dockerPruneRunner{uid: uid, state: "running"}
	removed, err = pruneWith(context.Background(), replacement, dockerEngine{})
	if err != nil || len(removed) != 0 || len(replacement.deleted) != 0 {
		t.Fatalf("running replacement: removed=%v deleted=%v err=%v", removed, replacement.deleted, err)
	}
}

type dockerPruneRunner struct {
	mu         sync.Mutex
	uid        string
	state      string
	deleteDown bool
	inspected  []string
	deleted    []string
}

func (r *dockerPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(r.uid + "\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.inspected = append(r.inspected, args[len(args)-1])
		state := r.state
		uid := r.uid
		r.mu.Unlock()
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":%q},"Config":{"Image":"redis","Labels":{%q:"true",%q:"0123456789abcdef"}}}]`, uid, state, managedLabel, creationLabel)), nil, nil
	case "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		down := r.deleteDown
		r.mu.Unlock()
		if down {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon unavailable"}
		}
		return nil, nil, nil
	case "version", "info":
		if r.deleteDown {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon unavailable"}
		}
		return []byte("ok"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDockerPruneClassifiesDeleteFailure(t *testing.T) {
	uid := strings.Repeat("c", 64)
	r := &dockerPruneRunner{uid: uid, state: "exited", deleteDown: true}
	_, err := pruneWith(context.Background(), r, dockerEngine{})
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
}

func TestDockerPruneRejectsNameListResults(t *testing.T) {
	r := &dockerPruneRunner{uid: "not-an-id", state: "exited"}
	if _, err := pruneWith(context.Background(), r, dockerEngine{}); err == nil {
		t.Fatal("name list result should be rejected")
	}
	if len(r.deleted) != 0 {
		t.Fatal("name list result issued a delete")
	}
}

func TestDockerDescriptorAndPlatformFailClosed(t *testing.T) {
	uid := strings.Repeat("d", 64)
	digest := "sha256:" + strings.Repeat("e", 64)
	data := []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Image":"redis:7","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef"}},"ImageManifestDescriptor":{"digest":%q,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}}]`, uid, managedLabel, reuseLabel, creationLabel, digest))
	info, err := (dockerEngine{}).parseInspect(data, uid)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if info.imageDigest != digest || info.image != "redis:7@"+digest || info.platform != "linux/arm64/v8" {
		t.Fatalf("descriptor metadata = image %q digest %q platform %q", info.image, info.imageDigest, info.platform)
	}
	badPlatform := []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Image":"redis:7","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef"}},"Platform":"linux"}]`, uid, managedLabel, reuseLabel, creationLabel))
	badInfo, err := (dockerEngine{}).parseInspect(badPlatform, uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkReuseOwned(badInfo, "redis:7", &config{name: "shared", platform: "linux/amd64"}); err == nil {
		t.Fatal("explicit architecture must fail closed without descriptor platform")
	}
	if (dockerEngine{}).parseImageExists([]byte(`[{"Id":"sha256:x"}]`), "linux/amd64") {
		t.Fatal("Docker image metadata without architecture must not satisfy explicit platform")
	}
	if !(dockerEngine{}).parseImageExists([]byte(`[{"Id":"sha256:x"}]`), "linux") {
		t.Fatal("an OS-only Docker selector must preserve image presence")
	}
	if (appleEngine{}).parseImageExists([]byte(`[{"variants":`), "linux/amd64") {
		t.Fatal("malformed Apple image metadata must not satisfy explicit platform")
	}
}

func TestTransientPostRunInspectIsRetried(t *testing.T) {
	oldDelay := inspectRetryDelay
	inspectRetryDelay = time.Millisecond
	defer func() { inspectRetryDelay = oldDelay }()
	r := &transientInspectRunner{}
	ctr := &Container{id: "transient", runner: r, eng: appleEngine{}, creation: "0123456789abcdef"}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if r.inspects < 2 || len(r.deleted) != 1 || r.deleted[0] != "transient" {
		t.Fatalf("inspects=%d deleted=%v, want retry then delete", r.inspects, r.deleted)
	}
}

type transientInspectRunner struct {
	mu       sync.Mutex
	inspects int
	deleted  []string
}

func (r *transientInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.inspects++
		n := r.inspects
		r.mu.Unlock()
		if n == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "not found"}
		}
		return reviewAppleInspect("transient", "stopped", "redis", "0123456789abcdef", "linux/amd64"), nil, nil
	case "delete":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestReuseDaemonDownIsClassifiedAndNotRetried(t *testing.T) {
	r := &daemonDownReuseRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("daemon-down"), WithReuse(), WithPullPolicy(PullNever),
		withRunner(r), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if r.runs != 0 {
		t.Fatalf("run attempts = %d, want no retry after daemon-down", r.runs)
	}
}

type daemonDownReuseRunner struct {
	mu       sync.Mutex
	inspects int
	runs     int
}

func (r *daemonDownReuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "image":
		return []byte(`[{"Id":"sha256:image"}]`), nil, nil
	case "inspect":
		r.inspects++
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon unavailable"}
	case "run":
		r.runs++
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon unavailable"}
	case "version", "system":
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon unavailable"}
	default:
		return nil, nil, nil
	}
}

func TestContainerUIDAccessorIsConcurrentSafe(t *testing.T) {
	uid := strings.Repeat("f", 64)
	ctr := &Container{}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctr.rememberImmutableID(uid)
		}()
		go func() {
			defer wg.Done()
			_ = ctr.immutableID()
		}()
	}
	wg.Wait()
	if ctr.immutableID() != uid {
		t.Fatalf("immutable ID = %q, want %q", ctr.immutableID(), uid)
	}
}
