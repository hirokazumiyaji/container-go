package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// imageCallIndex returns the position of the first image inspect or pull
// call, or -1 when no image work happened.
func imageCallIndex(calls [][]string) int {
	for i, call := range calls {
		if call[0] == "image" {
			return i
		}
	}
	return -1
}

func firstCallIndex(calls [][]string, subcommand string) int {
	for i, call := range calls {
		if call[0] == subcommand {
			return i
		}
	}
	return -1
}

func TestReuseStoppedRecycleKeepsGenerationWhenReplacementImageMissing(t *testing.T) {
	// The stopped generation is owned and compatible, but the requested
	// image is absent and PullNever may not fetch it. Deleting first would
	// leave the caller with nothing, so the generation must survive.
	f := &stoppedThenCreateRunner{fakeRunner: newTestRunner()}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullNever),
		withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("Run = %v, want ErrImageNotFound", err)
	}
	if f.deleted {
		t.Error("stopped generation was deleted although the replacement image is absent")
	}
	if f.created {
		t.Error("a replacement was created although the replacement image is absent")
	}
	if f.pullCalls != 0 {
		t.Errorf("pullCalls = %d, want 0 under PullNever", f.pullCalls)
	}
}

func TestReuseStoppedRecycleFetchesImageBeforeDeleting(t *testing.T) {
	// PullMissing may fetch, but it must fetch before the stopped
	// generation is discarded so a failed fetch keeps a usable container.
	f := &stoppedThenCreateRunner{fakeRunner: newTestRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullMissing),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
	if !f.deleted || !f.created {
		t.Fatalf("deleted = %v, created = %v, want the generation recycled", f.deleted, f.created)
	}
	imageAt := imageCallIndex(f.calls)
	deleteAt := firstCallIndex(f.calls, "delete")
	if imageAt < 0 {
		t.Fatal("no image inspect or pull was issued before the recycle")
	}
	if imageAt > deleteAt {
		t.Errorf("image work at call %d ran after the delete at %d: %v", imageAt, deleteAt, f.calls)
	}
}

func TestReuseStoppedRecycleKeepsGenerationOnImageMismatch(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "stopped", image: "nginx:1"}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded although the existing image does not match")
	}
	if f.callWith("delete") != nil {
		t.Error("an incompatible stopped generation was deleted")
	}
	if imageCallIndex(f.calls) >= 0 {
		t.Error("image work was issued although the stopped generation is not reusable")
	}
}

func TestReuseStoppedRecycleDeletesWithoutForceFlag(t *testing.T) {
	// A generation verified stopped is removed without --force, so one
	// that starts in the window is adopted instead of killed.
	f := &stoppedThenCreateRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	deleteCall := f.callWith("delete")
	if deleteCall == nil {
		t.Fatal("stopped generation was not deleted")
	}
	if slices.Contains(deleteCall, "--force") {
		t.Errorf("delete = %v, want no --force for a verified stopped generation", deleteCall)
	}
}

func TestReuseStoppedRecycleDefersToGenerationThatStarted(t *testing.T) {
	// The revalidation inside the delete path observes a running
	// generation: it must not delete, and the caller attaches instead.
	f := &stoppedThenRunningRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
	if f.callWith("delete") != nil {
		t.Errorf("delete = %v, want none once the generation started", f.callWith("delete"))
	}
	if firstCallIndex(f.calls, "run") >= 0 {
		t.Error("a second create was issued although the running generation was adopted")
	}
}

// stoppedThenRunningRunner reports the reusable generation as stopped for
// the first inspect and running from then on, so only the revalidation
// inside the delete path sees the start.
type stoppedThenRunningRunner struct {
	*fakeRunner
	inspects int
}

func (s *stoppedThenRunningRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.inspects++
		state := "running"
		if s.inspects == 1 {
			state = "stopped"
		}
		s.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], state, "redis:7-alpine")), nil, nil
	}
	if args[0] == "run" {
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.mu.Unlock()
		return nil, nil, &cli.CLIError{
			Args: args, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`,
		}
	}
	return s.fakeRunner.Run(ctx, args...)
}

// waitForReuseFlightsToDrain blocks until no shared ensure flight is
// still running, so a test can restore the package-level attach knobs
// without a background poll reading them.
func waitForReuseFlightsToDrain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reuseFlights.mu.Lock()
		pending := len(reuseFlights.inflight)
		reuseFlights.mu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("shared reuse ensure flights did not drain")
}

func TestReuseAttachTimeoutPreservesDeadlineExceeded(t *testing.T) {
	oldTimeout, oldPoll := reuseAttachTimeout, reusePollInterval
	reuseAttachTimeout = 40 * time.Millisecond
	reusePollInterval = time.Millisecond
	t.Cleanup(func() {
		waitForReuseFlightsToDrain(t)
		reuseAttachTimeout, reusePollInterval = oldTimeout, oldPoll
	})

	f := &attachRunner{fakeRunner: newTestRunner(), state: "created"}
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded although the container never became usable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want an error that still reports context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out waiting for a usable container") {
		t.Errorf("error = %v, want the attach timeout described", err)
	}
}

func TestReusePostAttachWorkOutlivesAttachBudget(t *testing.T) {
	// The attach budget bounds polling for a usable container only. File
	// application runs on the caller's context, so a copy that takes
	// longer than the attach budget still succeeds.
	oldTimeout := reuseAttachTimeout
	reuseAttachTimeout = 40 * time.Millisecond
	t.Cleanup(func() {
		waitForReuseFlightsToDrain(t)
		reuseAttachTimeout = oldTimeout
	})

	hostPath := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(hostPath, []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &slowAttachCopyRunner{fakeRunner: newTestRunner(), copyDelay: 150 * time.Millisecond}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/payload.txt"}),
		WithWaitStrategy(&contextProbeStrategy{runner: f}),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
	copyErr, waitErr := f.contextErrors()
	if copyErr != nil {
		t.Errorf("copy ran with an expired context: %v", copyErr)
	}
	if waitErr != nil {
		t.Errorf("wait ran with an expired context: %v", waitErr)
	}
}

type slowAttachCopyRunner struct {
	*fakeRunner
	copyDelay  time.Duration
	copyCtxErr error
	waitCtxErr error
}

// contextErrors returns the context failures observed by the copy and by
// the wait strategy.
func (s *slowAttachCopyRunner) contextErrors() (copyErr, waitErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyCtxErr, s.waitCtxErr
}

func (s *slowAttachCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], "running", "redis:7-alpine")), nil, nil
	case "cp":
		time.Sleep(s.copyDelay)
		if err := ctx.Err(); err != nil {
			s.mu.Lock()
			s.copyCtxErr = err
			s.calls = append(s.calls, args)
			s.mu.Unlock()
			return nil, nil, err
		}
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.mu.Unlock()
		return nil, nil, nil
	}
	return s.fakeRunner.Run(ctx, args...)
}

// contextProbeStrategy records the context it was handed after a short
// wait, so a test can tell a live post-attach context from the spent
// attach budget.
type contextProbeStrategy struct {
	runner *slowAttachCopyRunner
}

func (s *contextProbeStrategy) WaitUntilReady(ctx context.Context, _ wait.Target) error {
	time.Sleep(5 * time.Millisecond)
	if err := ctx.Err(); err != nil {
		s.runner.mu.Lock()
		s.runner.waitCtxErr = err
		s.runner.mu.Unlock()
		return err
	}
	return nil
}

func TestDeleteStoppedReuseCheckedRefusesChangedGeneration(t *testing.T) {
	// The delete path re-inspects its target and refuses when the
	// generation, ownership, or state is no longer the stopped generation
	// the caller verified.
	cases := []struct {
		name  string
		state string
		creat string
		reuse bool
	}{
		{name: "started", state: "running", creat: "0123456789abcdef"},
		{name: "replaced", state: "stopped", creat: "ffffffffffffffff"},
		{name: "not-a-reuse-generation", state: "stopped", creat: "0123456789abcdef", reuse: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fixedReuseInspectRunner{fakeRunner: newTestRunner(), state: tc.state, creation: tc.creat}
			if tc.reuse {
				f.reuse = true
			}
			f.imagePresent = true
			before := &engineInfo{
				state: StateStopped,
				labels: map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: "0123456789abcdef",
				},
			}
			cfg := &config{runner: f, eng: appleEngine{}, name: "myctr"}
			removed, err := deleteStoppedReuseChecked(context.Background(), cfg, before)
			if err != nil {
				t.Fatalf("deleteStoppedReuseChecked: %v", err)
			}
			if removed {
				t.Error("removed = true although the generation was no longer a stopped match")
			}
			if f.callWith("delete") != nil {
				t.Errorf("delete = %v, want none for a %s generation", f.callWith("delete"), tc.name)
			}
		})
	}
}

// fixedReuseInspectRunner always reports the same reuse generation, so a
// test can contrast the generation the caller verified with the one a
// fresh inspect returns.
type fixedReuseInspectRunner struct {
	*fakeRunner
	state    string
	creation string
	reuse    bool
}

func (s *fixedReuseInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.mu.Unlock()
		return []byte(reuseInspectJSONWithCreationAndReuse(
			args[len(args)-1], s.state, "redis:7-alpine", s.creation, s.reuse)), nil, nil
	}
	return s.fakeRunner.Run(ctx, args...)
}

// malformedDockerRunner prints a truncated ID from run, so the recovery
// path takes over with a name lookup, while the container it finds is a
// stopped reuse generation this run owns.
type malformedDockerRunner struct {
	*fakeRunner
	uid      string
	creation string
}

func (r *malformedDockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i, arg := range args {
			if i+1 < len(args) && arg == "--label" {
				if value := strings.TrimPrefix(args[i+1], creationLabel+"="); value != args[i+1] {
					r.creation = value
				}
			}
		}
		r.mu.Unlock()
		return []byte("truncated-id\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		creation := r.creation
		r.mu.Unlock()
		if creation == "" {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "no such container: myctr"}
		}
		labels := fmt.Sprintf(`{%q:%q,%q:%q,%q:%q,%q:%q}`,
			managedLabel, "true", sessionLabel, sessionID(), creationLabel, creation, reuseLabel, "true")
		return []byte(fmt.Sprintf(`[{
    "Id": %q,
    "State": {"Status": "exited"},
    "Config": {"Image": "redis:7-alpine", "Labels": %s}
  }]`, r.uid, labels)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestRecoverDockerRunOutputDeletesStoppedReuseWithoutForce(t *testing.T) {
	uid := strings.Repeat("ab", 32)
	f := &malformedDockerRunner{fakeRunner: newTestRunner(), uid: uid}
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("Run succeeded although docker printed no usable ID")
	}
	rmCall := f.callWith("rm")
	if rmCall == nil {
		t.Fatal("the recovered generation was not removed")
	}
	if slices.Contains(rmCall, "--force") {
		t.Errorf("rm = %v, want no --force for a verified stopped generation", rmCall)
	}
	if rmCall[len(rmCall)-1] != uid {
		t.Errorf("rm = %v, want delete by the verified immutable ID", rmCall)
	}
	// The ownership and state revalidation must run before the delete.
	inspectAt := firstCallIndex(f.calls, "inspect")
	rmAt := firstCallIndex(f.calls, "rm")
	if inspectAt < 0 || inspectAt > rmAt {
		t.Errorf("calls = %v, want revalidation before the delete", f.calls)
	}
}

func TestHasPublishedBindingRequiresExactNetipMatch(t *testing.T) {
	spec := publishSpec{hostAddr: "127.0.0.1", hostPort: 6379, containerPort: 6379, proto: "tcp"}
	cases := []struct {
		name  string
		bound []boundPort
		want  bool
	}{
		{
			name:  "exact",
			bound: []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 6379}},
			want:  true,
		},
		{
			name:  "unreported address is not a wildcard",
			bound: []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "", hostPort: 6379}},
			want:  false,
		},
		{
			name:  "ipv4 mapped ipv6 is a different address",
			bound: []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "::ffff:127.0.0.1", hostPort: 6379}},
			want:  false,
		},
		{
			name:  "other interface",
			bound: []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "192.168.1.5", hostPort: 6379}},
			want:  false,
		},
		{
			name:  "unspecified host is still an explicit request",
			bound: []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "0.0.0.0", hostPort: 6379}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec.hostAddr = "127.0.0.1"
			if got := hasPublishedBinding(tc.bound, spec); got != tc.want {
				t.Errorf("hasPublishedBinding(%+v) = %v, want %v", tc.bound, got, tc.want)
			}
		})
	}

	// An unspecified published address matches only that address, and a
	// request without an explicit address still matches any interface.
	spec.hostAddr = "0.0.0.0"
	unspecified := []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "0.0.0.0", hostPort: 6379}}
	if !hasPublishedBinding(unspecified, spec) {
		t.Error("0.0.0.0 request did not match a 0.0.0.0 binding")
	}
	other := []boundPort{{containerPort: 6379, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 6379}}
	if hasPublishedBinding(other, spec) {
		t.Error("0.0.0.0 request matched a 127.0.0.1 binding")
	}
	spec.hostAddr = ""
	if !hasPublishedBinding(other, spec) {
		t.Error("request without an explicit address did not match a reported binding")
	}
}

func TestAppleParseInspectKeepsOperatorPinnedDigest(t *testing.T) {
	indexDigest := "sha256:" + strings.Repeat("1", 64)
	childDigest := "sha256:" + strings.Repeat("2", 64)
	data := []byte(fmt.Sprintf(`[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine@%s", "descriptor": {"digest": %q, "mediaType": "application/vnd.oci.image.manifest.v1+json"}},
      "labels": {}
    },
    "status": {"state": "running", "networks": []}
  }
]`, indexDigest, childDigest))

	info, err := appleEngine{}.parseInspect(data, "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if want := "redis:7-alpine@" + indexDigest; info.image != want {
		t.Errorf("image = %q, want the operator-pinned %q", info.image, want)
	}
	if info.imageDigest != indexDigest {
		t.Errorf("imageDigest = %q, want the pinned index digest %q", info.imageDigest, indexDigest)
	}

	// Without a pinned reference the descriptor still completes the
	// reference, which is the only source of an immutable identity.
	unpinned := []byte(fmt.Sprintf(`[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine", "descriptor": {"digest": %q}},
      "labels": {}
    },
    "status": {"state": "running", "networks": []}
  }
]`, childDigest))
	info, err = appleEngine{}.parseInspect(unpinned, "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if want := "redis:7-alpine@" + childDigest; info.image != want {
		t.Errorf("image = %q, want the descriptor digest %q", info.image, want)
	}
	if info.imageDigest != childDigest {
		t.Errorf("imageDigest = %q, want %q", info.imageDigest, childDigest)
	}
}
