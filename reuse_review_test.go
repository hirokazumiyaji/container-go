package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func reviewLabels(creation string) map[string]string {
	return map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
}

func reviewLabelsJSON(creation string) string {
	data, _ := json.Marshal(reviewLabels(creation))
	return string(data)
}

func reviewAppleInspect(id, state, image, creation, platform string) []byte {
	platformData := map[string]string{}
	for i, part := range strings.Split(platform, "/") {
		switch i {
		case 0:
			platformData["os"] = part
		case 1:
			platformData["architecture"] = part
		case 2:
			platformData["variant"] = part
		}
	}
	platformJSON, _ := json.Marshal(platformData)
	return []byte(fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": %q},
      "labels": %s,
      "platform": %s,
      "publishedPorts": []
    },
    "status": {
      "state": %q,
      "networks": [{"ipv4Address": "192.168.64.3/24", "network": "default"}]
    }
  }
]`, id, id, image, reviewLabelsJSON(creation), platformJSON, state))
}

func reviewDockerInspect(uid, state, image, creation, platform string) []byte {
	return []byte(fmt.Sprintf(`[
  {
    "Id": %q,
    "Name": "/myctr",
    "State": {"Status": %q},
    "Config": {"Image": %q, "Labels": %s},
    "Platform": %q,
    "NetworkSettings": {"IPAddress": "172.17.0.2", "Ports": {}}
  }
]`, uid, state, image, reviewLabelsJSON(creation), platform))
}

type reviewStoppedRunner struct {
	*fakeRunner
	mu                sync.Mutex
	inspectCount      int
	deleted           bool
	created           bool
	pullCalls         int
	stoppedGeneration string
	createdGeneration string
}

func newReviewStoppedRunner() *reviewStoppedRunner {
	return &reviewStoppedRunner{
		fakeRunner:        newTestRunner(),
		stoppedGeneration: "aaaaaaaaaaaaaaaa",
		createdGeneration: "bbbbbbbbbbbbbbbb",
	}
}

func (r *reviewStoppedRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		if len(args) > 1 && args[1] == "pull" {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.pullCalls++
			r.mu.Unlock()
			return nil, nil, nil
		}
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectCount++
		count := r.inspectCount
		deleted, created := r.deleted, r.created
		stoppedGeneration, createdGeneration := r.stoppedGeneration, r.createdGeneration
		r.mu.Unlock()
		if created {
			return reviewAppleInspect("myctr", "running", "redis:7-alpine", createdGeneration, "linux/amd64"), nil, nil
		}
		if deleted {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		if count == 1 {
			return reviewAppleInspect("myctr", "running", "redis:7-alpine", "aaaaaaaaaaaaaaaa", "linux/amd64"), nil, nil
		}
		return reviewAppleInspect("myctr", "stopped", "redis:7-alpine", stoppedGeneration, "linux/amd64"), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = true
		r.mu.Unlock()
		return nil, nil, nil
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.created = true
		r.mu.Unlock()
		return []byte("myctr\n"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReusePullAlwaysStoppedReinspectionRecreates(t *testing.T) {
	f := newReviewStoppedRunner()
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil {
		t.Fatal("Run returned nil container")
	}
	f.mu.Lock()
	deleted, created, pulls := f.deleted, f.created, f.pullCalls
	f.mu.Unlock()
	if !deleted || !created {
		t.Fatalf("deleted=%v created=%v, want stopped reuse recreate", deleted, created)
	}
	if pulls != 1 {
		t.Fatalf("pulls=%d, want 1", pulls)
	}
}

func TestReusePullAlwaysStoppedReplacementRecreates(t *testing.T) {
	f := newReviewStoppedRunner()
	f.stoppedGeneration = "bbbbbbbbbbbbbbbb"
	f.createdGeneration = "cccccccccccccccc"
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.mu.Lock()
	deleted, created := f.deleted, f.created
	f.mu.Unlock()
	if !deleted || !created {
		t.Fatalf("deleted=%v created=%v, want replacement recreate", deleted, created)
	}
}

type reviewUnownedStoppedRunner struct {
	*fakeRunner
	mu      sync.Mutex
	inspect int
	deleted int
}

func (r *reviewUnownedStoppedRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspect++
		n := r.inspect
		r.mu.Unlock()
		if n == 1 {
			return reviewAppleInspect("myctr", "running", "redis:7-alpine", "aaaaaaaaaaaaaaaa", "linux/amd64"), nil, nil
		}
		return []byte(fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"labels":{"%s":"true"}},"status":{"state":"stopped","networks":[]}}]`, reuseLabel)), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted++
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReusePullAlwaysStoppedUnownedFailsWithoutDelete(t *testing.T) {
	f := &reviewUnownedStoppedRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "managed") {
		t.Fatalf("error = %v, want ownership error", err)
	}
	f.mu.Lock()
	deleted := f.deleted
	f.mu.Unlock()
	if deleted != 0 {
		t.Fatalf("delete attempts = %d, want none for unowned stopped container", deleted)
	}
}

type reviewDockerRunner struct {
	*fakeRunner
	mu            sync.Mutex
	uid           string
	newUID        string
	platform      string
	state         string
	cpCalls       int
	replaceOnCopy bool
}

func newReviewDockerRunner(uid, platform string) *reviewDockerRunner {
	return &reviewDockerRunner{fakeRunner: newTestRunner(), uid: uid, platform: platform, state: "running"}
}

func (r *reviewDockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		if len(args) > 1 && args[1] == "pull" {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.pullCalls++
			r.mu.Unlock()
			return nil, nil, nil
		}
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		uid := r.uid
		platform := r.platform
		state := r.state
		r.mu.Unlock()
		return reviewDockerInspect(uid, state, "redis:7-alpine", "aaaaaaaaaaaaaaaa", platform), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.cpCalls++
		if r.replaceOnCopy && r.newUID != "" {
			r.uid = r.newUID
		}
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReuseAttachFilesUseDockerImmutableUID(t *testing.T) {
	uid := strings.Repeat("a", 64)
	f := newReviewDockerRunner(uid, "linux/amd64")
	f.imagePresent = true
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPlatform("linux/amd64"),
		WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := f.callWith("cp")
	if call == nil || call[2] != uid+":/fixture.txt" {
		t.Fatalf("cp target = %v, want immutable UID target %q", call, uid)
	}
}

type reviewAppleReplacementRunner struct {
	*fakeRunner
	mu       sync.Mutex
	inspects int
	cpCalls  int
}

func (r *reviewAppleReplacementRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspects++
		generation := "aaaaaaaaaaaaaaaa"
		if r.inspects >= 3 {
			generation = "bbbbbbbbbbbbbbbb"
		}
		r.mu.Unlock()
		return reviewAppleInspect(args[len(args)-1], "running", "redis:7-alpine", generation, "linux/amd64"), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.cpCalls++
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReuseAttachFilesFailsClosedAfterDockerReplacement(t *testing.T) {
	oldUID := strings.Repeat("a", 64)
	newUID := strings.Repeat("b", 64)
	f := newReviewDockerRunner(oldUID, "linux/amd64")
	f.newUID = newUID
	f.replaceOnCopy = true
	f.imagePresent = true
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPlatform("linux/amd64"),
		WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want ErrGenerationReplaced", err)
	}
	call := f.callWith("cp")
	if call == nil || call[2] != oldUID+":/fixture.txt" {
		t.Fatalf("cp call = %v, want old immutable UID", call)
	}
}

func TestReuseAttachFilesRejectsAppleReplacementBeforeCopy(t *testing.T) {
	f := &reviewAppleReplacementRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want ErrGenerationReplaced", err)
	}
	if f.cpCalls != 0 {
		t.Fatalf("cp calls = %d, want no copy after replacement", f.cpCalls)
	}
}

func TestReusePullAlwaysRejectsPlatformMismatch(t *testing.T) {
	f := newReviewDockerRunner(strings.Repeat("a", 64), "linux/arm64")
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways), WithPlatform("linux/amd64"),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("error = %v, want platform mismatch", err)
	}
}

func TestReusePullAlwaysUsesMatchingPlatform(t *testing.T) {
	f := newReviewDockerRunner(strings.Repeat("a", 64), "linux/amd64")
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways), WithPlatform("linux/amd64"),
		withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := f.callWith("pull")
	if call == nil || !strings.Contains(strings.Join(call, " "), "--platform linux/amd64") {
		t.Fatalf("pull call = %v, want platform-specific fetch", call)
	}
}

type reviewReplaceOnWait struct {
	replace func()
}

func (s *reviewReplaceOnWait) WaitUntilReady(context.Context, wait.Target) error {
	s.replace()
	return nil
}

func TestReuseRejectsReplacementAfterWait(t *testing.T) {
	oldUID := strings.Repeat("a", 64)
	newUID := strings.Repeat("b", 64)
	f := newReviewDockerRunner(oldUID, "linux/amd64")
	f.imagePresent = true
	strategy := &reviewReplaceOnWait{replace: func() {
		f.mu.Lock()
		f.uid = newUID
		f.mu.Unlock()
	}}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithWaitStrategy(strategy),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Run = (%v, %v), want replacement error", ctr, err)
	}
}

func TestReuseDoesNotReturnStoppedAfterWait(t *testing.T) {
	f := newReviewDockerRunner(strings.Repeat("a", 64), "linux/amd64")
	f.imagePresent = true
	strategy := &reviewReplaceOnWait{replace: func() {
		f.mu.Lock()
		f.state = "exited"
		f.mu.Unlock()
	}}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithWaitStrategy(strategy),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil || ctr != nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Run = (%v, %v), want stopped-state error", ctr, err)
	}
}

type reviewReuseCopyFailureRunner struct {
	*fakeRunner
	mu         sync.Mutex
	created    bool
	generation string
	deleteErrs int
	cpErr      error
	termErr    error
}

func (r *reviewReuseCopyFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		created := r.created
		generation := r.generation
		r.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		return reviewAppleInspect("myctr", "running", "redis:7-alpine", generation, "linux/amd64"), nil, nil
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.created = true
		for i := range args {
			if i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				r.generation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		r.mu.Unlock()
		return []byte("myctr\n"), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.cpErr
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleteErrs++
		r.mu.Unlock()
		return nil, nil, r.termErr
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReuseCreateCopyFailureDoesNotDeleteSharedContainer(t *testing.T) {
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "copy failed"}
	termErr := &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "delete failed"}
	f := &reviewReuseCopyFailureRunner{
		fakeRunner: newTestRunner(),
		cpErr:      copyErr,
		termErr:    termErr,
	}
	f.imagePresent = true
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !reviewHasCLIError(err, "copy failed") {
		t.Fatalf("error = %v, want copy failure", err)
	}
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want no implicit shared cleanup", err)
	}
	f.mu.Lock()
	deletes := f.deleteErrs
	f.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("delete attempts = %d, want no rollback of shared reuse container", deletes)
	}
}

type reviewRollbackRunner struct {
	*fakeRunner
	copyErr   error
	deleteErr error
}

func (r *reviewRollbackRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.copyErr
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.deleteErr
	}
	return r.fakeRunner.Run(ctx, args...)
}

func reviewHasCLIError(err error, stderr string) bool {
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		return reviewCLIErrorContains(cleanupErr.Err, stderr) || reviewCLIErrorContains(cleanupErr.CleanupErr, stderr)
	}
	return reviewCLIErrorContains(err, stderr)
}

func reviewCLIErrorContains(err error, stderr string) bool {
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr) && strings.Contains(cliErr.Stderr, stderr)
}

func TestRunCopyFailurePreservesCleanupCLIError(t *testing.T) {
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "copy failed"}
	deleteErr := &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "delete failed"}
	f := &reviewRollbackRunner{fakeRunner: newTestRunner(), copyErr: copyErr, deleteErr: deleteErr}
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(appleEngine{}))
	if !errors.As(err, new(*CleanupError)) || !reviewHasCLIError(err, "copy failed") || !reviewHasCLIError(err, "delete failed") {
		t.Fatalf("error = %v, want CleanupError preserving copy and delete failures", err)
	}
}
