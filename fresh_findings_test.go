package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewReaperAutonomousRetryAfterCooldown(t *testing.T) {
	r := newReaper("unused", "rm")
	defer r.closeStdin()
	var attempts atomic.Int32
	r.backoff = func(int) time.Duration { return 15 * time.Millisecond }
	r.command = func() *exec.Cmd {
		if attempts.Add(1) <= maxReaperSpawnFailures {
			return exec.Command("/definitely/missing/reaper")
		}
		return exec.Command("/bin/sh", "-c", "cat")
	}
	uid := strings.Repeat("a", 64)
	if err := r.register(uid, ""); err == nil {
		t.Fatal("initial registration unexpectedly succeeded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		gaveUp := r.gaveUp
		r.mu.Unlock()
		if !gaveUp && attempts.Load() > maxReaperSpawnFailures && r.processLiveLockedForTest() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("autonomous retry did not replace the child: attempts=%d", attempts.Load())
}

func TestReviewDockerReaperRequiresContainerState(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then echo '  \"Id\": \"'\"$3\"'\"'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	uid := strings.Repeat("f", 64)
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper",
		bin, "rm", breQuote(creationLabel), breQuote(managedLabel), breQuote(sessionLabel), "1", "1")
	cmd.Stdin = strings.NewReader("+\t" + uid + "\t\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper protocol: %v (%s)", err, output)
	}
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "rm --force "+uid) {
		t.Fatalf("reaper removed an ID without container-shaped State: %q", data)
	}
}

func (r *reaper) processLiveLockedForTest() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.processLiveLocked()
}

func TestReviewReaperImmediateExitEntersBackoff(t *testing.T) {
	r := newReaper("unused", "rm")
	defer r.closeStdin()
	var attempts atomic.Int32
	r.backoff = func(int) time.Duration { return 25 * time.Millisecond }
	r.command = func() *exec.Cmd {
		attempts.Add(1)
		return exec.Command("/bin/sh", "-c", "exit 0")
	}
	uid := strings.Repeat("b", 64)
	_ = r.register(uid, "")
	deadline := time.Now().Add(2 * time.Second)
	failures := 0
	for time.Now().Before(deadline) {
		r.mu.Lock()
		gaveUp := r.gaveUp
		failures = r.spawnFailures
		r.mu.Unlock()
		if gaveUp && failures >= maxReaperSpawnFailures {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("immediate child exits were not counted for backoff: attempts=%d failures=%d", attempts.Load(), failures)
}

func TestReviewDockerInspectRequiresContainerShapeAndType(t *testing.T) {
	if got := (dockerEngine{}).inspectArgs("myctr"); strings.Join(got, " ") != "inspect --type=container myctr" {
		t.Fatalf("inspect args = %v, want container type", got)
	}
	uid := strings.Repeat("c", 64)
	data := []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/other","State":{"Status":"running"}},`+
		`{"Id":%q,"Name":"/myctr"}]`, uid, strings.Repeat("d", 64)))
	if _, err := (dockerEngine{}).parseInspect(data, "myctr"); err == nil {
		t.Fatal("name matched an object without container-shaped State")
	}
	if !strings.Contains(reaperScript, "inspect --type=container") {
		t.Fatal("reaper does not constrain Docker inspect to containers")
	}
}

type reviewBlockingInspectRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (r *reviewBlockingInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "inspect" {
		return nil, nil, nil
	}
	r.calls.Add(1)
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return []byte(fmt.Sprintf(`[{"id":"cache-cancel","configuration":{"id":"cache-cancel","image":{"reference":"redis:7-alpine"},"labels":{"%s":"true","%s":%q,"%s":"%s"}},"status":{"state":"running","networks":[]}}]`, managedLabel, sessionLabel, sessionID(), creationLabel, "aaaaaaaaaaaaaaaa")), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func TestReviewInspectSerializationHonorsCallerCancellation(t *testing.T) {
	runner := &reviewBlockingInspectRunner{started: make(chan struct{}), release: make(chan struct{})}
	ctr := &Container{id: "cache-cancel", runner: runner, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	firstDone := make(chan error, 1)
	go func() {
		_, err := ctr.cachedInfo(context.Background())
		firstDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first inspect did not start")
	}
	secondCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := ctr.cachedInfo(secondCtx)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second inspect error = %v, want deadline", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled inspect remained blocked behind the first inspect")
	}
	close(runner.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first inspect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first inspect did not finish")
	}
}

type reviewCancelingBackendRunner struct {
	*reviewBlockingInspectRunner
}

func (r *reviewCancelingBackendRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "inspect" {
		return nil, nil, ctx.Err()
	}
	return r.reviewBlockingInspectRunner.Run(ctx, args...)
}

func TestReviewTerminateDoesNotWaitBehindInspectSerialization(t *testing.T) {
	runner := &reviewCancelingBackendRunner{reviewBlockingInspectRunner: &reviewBlockingInspectRunner{started: make(chan struct{}), release: make(chan struct{})}}
	ctr := &Container{id: "cancel-term", uid: strings.Repeat("9", 64), runner: runner, eng: dockerEngine{}}
	firstDone := make(chan error, 1)
	go func() {
		_, err := ctr.State(context.Background())
		firstDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first inspect did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	terminateDone := make(chan error, 1)
	go func() { terminateDone <- ctr.Terminate(ctx) }()
	select {
	case err := <-terminateDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Terminate error = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled Terminate waited behind inspect serialization")
	}
	close(runner.release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first inspect did not finish")
	}
}

func TestReviewKeepReturnsPartialHandleOnCopyFailure(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	runner := &rollbackErrorRunner{fakeRunner: base, copyErr: errors.New("copy failed")}
	hostPath := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(hostPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-copy"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/input"}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want partial handle and copy error", ctr, err)
	}
	if ctr.ID() != "keep-copy" {
		t.Fatalf("partial handle ID = %q", ctr.ID())
	}
	if _, err := ctr.State(context.Background()); err != nil {
		t.Fatalf("partial handle is not verified: %v", err)
	}
}

func TestReviewKeepReturnsPartialHandleOnReadinessFailure(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	runner := &rollbackErrorRunner{fakeRunner: base}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-ready"), withRunner(runner), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("readiness failed")}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want partial handle and readiness error", ctr, err)
	}
	if _, err := ctr.State(context.Background()); err != nil {
		t.Fatalf("partial handle is not verified: %v", err)
	}
}

func TestReviewPreRegistrationRollsBackFailedInitialEntry(t *testing.T) {
	isolateReviewGlobalReapers(t)
	bin := "review-pre-registration-rollback"
	r := getGlobalReaper(bin, "rm")
	r.command = func() *exec.Cmd { return exec.Command("/definitely/missing/reaper") }
	reg, err := registerContainerWithGlobalReaper(bin, "rm", strings.Repeat("e", 64), "", "", false, false)
	if err == nil || reg == nil {
		t.Fatalf("registration = (%v, %v), want staged rollback handle and error", reg, err)
	}
	r.mu.Lock()
	staged := len(r.entries)
	r.mu.Unlock()
	if staged != 1 {
		t.Fatalf("staged entries = %d, want one rollback target", staged)
	}
	unregisterWithGlobalReaper(reg)
	r.mu.Lock()
	remaining := len(r.entries)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("rollback left %d staged entries", remaining)
	}
}
