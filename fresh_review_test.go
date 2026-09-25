package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const freshReviewDockerUID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type freshReviewOperationRunner struct {
	mu          sync.Mutex
	calls       [][]string
	inspectJSON string
}

func (r *freshReviewOperationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	switch args[0] {
	case "inspect":
		if r.inspectJSON != "" {
			return []byte(r.inspectJSON), nil, nil
		}
	case "cp":
		// Materialize a container-to-host copy so CopyFileFromContainer can
		// complete its normal post-run validation.
		if len(args) == 3 && strings.Contains(args[1], ":") {
			if err := os.WriteFile(args[2], []byte("copied"), 0o600); err != nil {
				return nil, nil, err
			}
		}
	case "exec":
		return []byte("ok"), nil, nil
	case "logs":
		return []byte("diagnostic"), nil, nil
	}
	return nil, nil, nil
}

func (r *freshReviewOperationRunner) snapshotCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

func (r *freshReviewOperationRunner) hasTarget(target string) bool {
	for _, args := range r.snapshotCalls() {
		for _, arg := range args {
			if arg == target || strings.HasPrefix(arg, target+":") {
				return true
			}
		}
	}
	return false
}

type freshReviewStreamRunner struct {
	*freshReviewOperationRunner
	args []string
}

func (r *freshReviewStreamRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	r.args = append([]string(nil), args...)
	return io.NopCloser(strings.NewReader("streamed")), nil
}

func TestFreshReviewDockerOperationsUseImmutableUID(t *testing.T) {
	runner := &freshReviewOperationRunner{}
	ctr := &Container{id: "logical-name", uid: freshReviewDockerUID, runner: runner, eng: dockerEngine{}}
	if ctr.ID() != "logical-name" {
		t.Fatalf("ID = %q, want logical name", ctr.ID())
	}

	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := ctr.CopyToContainer(context.Background(), writeFreshReviewFile(t), "/tmp/in"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	rc, err := ctr.CopyFileFromContainer(context.Background(), "/tmp/out")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	_ = rc.Close()
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	logs, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = logs.Close()
	if tail := ctr.logTail(context.Background()); tail != "diagnostic" {
		t.Fatalf("logTail = %q, want diagnostic", tail)
	}

	if !runner.hasTarget(freshReviewDockerUID) {
		t.Fatalf("operations did not use Docker UID: %v", runner.snapshotCalls())
	}
	for _, args := range runner.snapshotCalls() {
		for _, arg := range args {
			if arg == "logical-name" || strings.HasPrefix(arg, "logical-name:") {
				t.Fatalf("operation addressed logical name: %v", args)
			}
		}
	}

	streamRunner := &freshReviewStreamRunner{freshReviewOperationRunner: &freshReviewOperationRunner{}}
	streamCtr := &Container{id: "logical-name", uid: freshReviewDockerUID, runner: streamRunner, eng: dockerEngine{}}
	stream, err := streamCtr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	_ = stream.Close()
	if !containsArg(streamRunner.args, freshReviewDockerUID) || containsArg(streamRunner.args, "logical-name") {
		t.Fatalf("FollowLogs args = %v, want immutable UID", streamRunner.args)
	}
}

func writeFreshReviewFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestFreshReviewAppleOperationsKeepLogicalName(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	runner := &freshReviewOperationRunner{
		inspectJSON: issue83ReviewAppleInspect("apple-name", creation, "running"),
	}
	ctr := &Container{id: "apple-name", runner: runner, eng: appleEngine{}, creation: creation}
	if got := ctr.operationTarget(); got != "apple-name" {
		t.Fatalf("operationTarget = %q, want logical name", got)
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Apple Stop: %v", err)
	}
	if !runner.hasTarget("apple-name") {
		t.Fatalf("Apple Stop did not use logical name: %v", runner.snapshotCalls())
	}
}

func TestFreshReviewDockerOperationFailsClosedWithoutUID(t *testing.T) {
	runner := &freshReviewOperationRunner{}
	ctr := &Container{id: "logical-name", runner: runner, eng: dockerEngine{}}
	if err := ctr.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop without immutable UID succeeded")
	}
	if len(runner.snapshotCalls()) != 0 {
		t.Fatalf("Stop without immutable UID issued a backend call: %v", runner.snapshotCalls())
	}
}

type freshReviewInspectRunner struct{}

func (freshReviewInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(`[{"Id":"` + freshReviewDockerUID + `","Name":"/logical-name","State":{"Status":"running"},"Config":{"Image":"replacement:latest","Labels":{"` + managedLabel + `":"true","` + reuseLabel + `":"true","` + creationLabel + `":"bbbbbbbbbbbbbbbb"}}}]`), nil, nil
	}
	return nil, nil, nil
}

func TestFreshReviewInspectDoesNotBindUnverifiedDockerUID(t *testing.T) {
	ctr := &Container{
		id:          "logical-name",
		creation:    "aaaaaaaaaaaaaaaa",
		runner:      freshReviewInspectRunner{},
		eng:         dockerEngine{},
		nameInspect: true,
	}
	info, err := ctr.inspectFresh(context.Background())
	if err != nil {
		t.Fatalf("inspectFresh: %v", err)
	}
	if info.uid != freshReviewDockerUID {
		t.Fatalf("local inspect UID = %q, want replacement UID", info.uid)
	}
	if ctr.uid != "" {
		t.Fatalf("unverified same-name replacement published UID %q", ctr.uid)
	}
	if _, err := ctr.verifiedOperationTarget(); err == nil {
		t.Fatal("handle without a verified UID became operational")
	}
}

type freshReviewReuseRunner struct {
	mu          sync.Mutex
	deleteCalls int
	failInspect bool
}

func (r *freshReviewReuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "image":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case "run":
		return []byte("shared\n"), nil, nil
	case "inspect":
		if r.failInspect {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected inspect failure"}
		}
		return []byte(`[{"id":"shared","configuration":{"id":"shared","image":{"reference":"redis:7-alpine"},"labels":{"` + managedLabel + `":"true","` + reuseLabel + `":"true","` + creationLabel + `":"aaaaaaaaaaaaaaaa"}},"status":{"state":"running","networks":[]}}]`), nil, nil
	case "cp":
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected copy failure"}
	case "delete", "rm":
		r.deleteCalls++
	}
	return nil, nil, nil
}

func (r *freshReviewReuseRunner) deletes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteCalls
}

func TestFreshReviewReusePostCreateFailurePreservesPublishedGeneration(t *testing.T) {
	for _, failInspect := range []bool{false, true} {
		t.Run(fmt.Sprintf("inspect_failure=%v", failInspect), func(t *testing.T) {
			runner := &freshReviewReuseRunner{failInspect: failInspect}
			cfg := &config{
				runner:   runner,
				eng:      appleEngine{},
				name:     "shared",
				reuse:    true,
				creation: "aaaaaaaaaaaaaaaa",
				files:    []File{{HostPath: writeFreshReviewFile(t), ContainerPath: "/tmp/in"}},
			}
			_, err := reuseCreate(context.Background(), "redis:7-alpine", cfg)
			if err == nil {
				t.Fatal("reuseCreate unexpectedly succeeded")
			}
			if runner.deletes() != 0 {
				t.Fatalf("post-create failure deleted shared generation %d times", runner.deletes())
			}
		})
	}
}

func TestFreshReviewDockerPlatformDescriptorIsUsed(t *testing.T) {
	data := []byte(`[{"Id":"` + freshReviewDockerUID + `","Name":"/logical-name","Platform":"linux","ImageManifestDescriptor":{"platform":{"os":"linux","architecture":"arm64","variant":"v8"}},"State":{"Status":"running"}}]`)
	info, err := (dockerEngine{}).parseInspect(data, freshReviewDockerUID)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if info.platform != "linux/arm64/v8" {
		t.Fatalf("platform = %q, want linux/arm64/v8", info.platform)
	}
	if !(dockerEngine{}).platformCompatible("linux/arm64", info.platform) {
		t.Fatal("descriptor platform was not architecture-aware")
	}
	if (dockerEngine{}).platformCompatible("linux/amd64", "linux") {
		t.Fatal("OS-only Docker inspect incorrectly satisfied an architecture selector")
	}
}

func TestFreshReviewAppleImagePlatformFailsClosed(t *testing.T) {
	inputs := []string{
		`[{"variants":[]}]`,
		`[{"variants":[{"platform":{"os":"linux","architecture":17}}]}]`,
		`[{"variants":[{"platform":{}}]}]`,
		`[{"variants":[{"platform":"not-an-object"}]}]`,
	}
	for _, data := range inputs {
		for _, platform := range []string{"linux/arm64", "linux/arm/v7"} {
			if (appleEngine{}).parseImageExists([]byte(data), platform) {
				t.Errorf("parseImageExists(%s, %q) = true, want fail closed", data, platform)
			}
		}
		if !(appleEngine{}).parseImageExists([]byte(data), "linux") {
			t.Errorf("parseImageExists(%s, linux) = false, want OS-only presence", data)
		}
	}
}

func TestFreshReviewAppleEmptyPlatformIsUnconstrained(t *testing.T) {
	if !(appleEngine{}).platformCompatible("", "linux/arm64") {
		t.Fatal("empty Apple platform selector was treated as constrained")
	}
	if !(appleEngine{}).platformCompatible("", "") {
		t.Fatal("empty Apple platform selector rejected an unconstrained match")
	}
	malformed := []byte(`[{"variants":[{"platform":"not-an-object"}]}]`)
	if !(appleEngine{}).parseImageExists(malformed, "") {
		t.Fatal("empty Apple platform selector did not preserve image presence")
	}
}

func TestFreshReviewReaperAppleDeleteUsesNameLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple reaper name locks use POSIX helpers")
	}
	name := "reaper-review-" + newContainerName()
	const creation = "0123456789abcdef"
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	inspectStarted := filepath.Join(dir, "inspect-started")
	releaseInspect := filepath.Join(dir, "release-inspect")
	bin := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  : > " + inspectStarted + "\n" +
		"  while [ ! -e " + releaseInspect + " ]; do sleep 0.05; done\n" +
		"  echo '  \"" + creationLabel + "\": \"" + creation + "\"'\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()

	waitForFreshReviewPath(t, inspectStarted)
	lockResult := make(chan struct {
		unlock func()
		err    error
	}, 1)
	go func() {
		unlock, err := lockName(context.Background(), name)
		lockResult <- struct {
			unlock func()
			err    error
		}{unlock, err}
	}()
	select {
	case result := <-lockResult:
		if result.unlock != nil {
			result.unlock()
		}
		t.Fatalf("name lock was available while reaper held it: %v", result.err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.WriteFile(releaseInspect, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLogLines(t, logPath, "inspect "+name, "delete --force "+name)
	select {
	case result := <-lockResult:
		if result.err != nil {
			t.Fatalf("lockName after reaper: %v", result.err)
		}
		result.unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("name lock did not become available after reaper delete")
	}
	waitForFreshReviewReaperExit(t, r)
}

func waitForFreshReviewReaperExit(t *testing.T, r *reaper) {
	t.Helper()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper child did not exit")
	}
}

func TestFreshReviewAppleCreateUsesSameNameLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple name locks use POSIX helpers")
	}
	name := "create-review-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	runner := &freshReviewOperationRunner{}
	cfg := &config{name: name, runner: runner, eng: appleEngine{}}
	done := make(chan error, 1)
	go func() {
		_, _, err := runCreateLocked(context.Background(), cfg, "run")
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("create entered while name lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("create after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("create did not finish after lock release")
	}
}

func waitForFreshReviewPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
