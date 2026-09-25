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
	"github.com/hirokazumiyaji/container-go/wait"
)

const reviewCreationA = "aaaaaaaaaaaaaaaa"
const reviewCreationB = "bbbbbbbbbbbbbbbb"

type reviewOperationRunner struct {
	mu       sync.Mutex
	calls    [][]string
	inspectA string
	inspectB string
}

func (r *reviewOperationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	n := 0
	for _, call := range r.calls {
		if len(call) > 0 && call[0] == "inspect" {
			n++
		}
	}
	r.mu.Unlock()
	switch args[0] {
	case "inspect":
		if n > 1 && r.inspectB != "" {
			return []byte(r.inspectB), nil, nil
		}
		return []byte(r.inspectA), nil, nil
	case "cp":
		if len(args) == 3 && !strings.Contains(args[2], ":") {
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

func (r *reviewOperationRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	return io.NopCloser(strings.NewReader("streamed")), nil
}

func (r *reviewOperationRunner) hasOp(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, args := range r.calls {
		if len(args) > 0 && args[0] == name {
			if name == "inspect" {
				continue
			}
			return true
		}
	}
	return false
}

func reviewAppleInspect(name, creation, state string, ports string) string {
	if ports == "" {
		ports = "[]"
	}
	return fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine"},"labels":{"%s":"true","%s":"true","%s":%q},"publishedPorts":%s},"status":{"state":%q,"networks":[]}}]`,
		name, name, managedLabel, reuseLabel, creationLabel, creation, ports, state)
}

func TestReviewAppleOperationsRecheckGenerationBeforeNameUse(t *testing.T) {
	const name = "review-operation"
	host := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(host, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		call func(*Container) error
	}{
		{"stop", func(c *Container) error { return c.Stop(context.Background(), nil) }},
		{"copy-to", func(c *Container) error { return c.CopyToContainer(context.Background(), host, "/tmp/input") }},
		{"copy-from", func(c *Container) error {
			_, err := c.CopyFileFromContainer(context.Background(), "/tmp/output")
			return err
		}},
		{"exec", func(c *Container) error {
			_, _, err := c.Exec(context.Background(), []string{"true"})
			return err
		}},
		{"logs", func(c *Container) error {
			_, err := c.Logs(context.Background())
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &reviewOperationRunner{
				inspectA: reviewAppleInspect(name, reviewCreationB, "running", ""),
				inspectB: reviewAppleInspect(name, reviewCreationB, "running", ""),
			}
			ctr := &Container{id: name, runner: runner, eng: appleEngine{}, creation: reviewCreationA}
			err := tc.call(ctr)
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("operation error = %v, want ErrGenerationReplaced", err)
			}
			if runner.hasOp("stop") || runner.hasOp("cp") || runner.hasOp("exec") || runner.hasOp("logs") {
				t.Fatalf("replacement reached a backend operation: %v", runner.calls)
			}
		})
	}
}

func TestReviewAppleLogTailAndStreamingBlockReplacementWhileOpen(t *testing.T) {
	const name = "review-stream"
	runner := &reviewOperationRunner{
		inspectA: reviewAppleInspect(name, reviewCreationB, "running", ""),
		inspectB: reviewAppleInspect(name, reviewCreationB, "running", ""),
	}
	ctr := &Container{id: name, runner: runner, eng: appleEngine{}, creation: reviewCreationA}
	if got := ctr.logTail(context.Background()); got != "" {
		t.Fatalf("replacement log tail = %q, want empty", got)
	}
	if runner.hasOp("logs") {
		t.Fatalf("replacement log tail issued a logs call: %v", runner.calls)
	}

	runner.inspectA = reviewAppleInspect(name, reviewCreationA, "running", "")
	runner.inspectB = ""
	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	lockResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		unlock, err := lockName(ctx, name)
		if unlock != nil {
			unlock()
		}
		lockResult <- err
	}()
	select {
	case err := <-lockResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("replacement lock while stream is open = %v, want deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement lock did not wait for stream completion")
	}
	state, err := ctr.State(context.Background())
	if err != nil || state != StateRunning {
		t.Fatalf("shared State while stream is open = (%s, %v)", state, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream close: %v", err)
	}
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("replacement lock after stream close: %v", err)
	}
	unlock()
}

type reviewPruneRunner struct {
	list    string
	inspect string
	mu      sync.Mutex
	deletes []string
}

func (r *reviewPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ls":
		return []byte(r.list), nil, nil
	case "inspect":
		return []byte(r.inspect), nil, nil
	case "delete":
		r.mu.Lock()
		r.deletes = append(r.deletes, args[len(args)-1])
		r.mu.Unlock()
	}
	return nil, nil, nil
}

func TestReviewApplePruneRejectsListGenerationReplacement(t *testing.T) {
	const name = "review-prune"
	list := fmt.Sprintf(`[{"id":%q,"configuration":{"labels":{"%s":"true","%s":"true","%s":%q}},"status":{"state":"stopped","networks":[]}}]`,
		name, managedLabel, reuseLabel, creationLabel, reviewCreationA)
	runner := &reviewPruneRunner{
		list:    list,
		inspect: reviewAppleInspect(name, reviewCreationB, "stopped", ""),
	}
	removed, err := pruneWith(context.Background(), runner, appleEngine{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 0 || len(runner.deletes) != 0 {
		t.Fatalf("replacement was pruned: removed=%v deletes=%v", removed, runner.deletes)
	}
}

func TestReviewAppleGroupPruneRequiresReuseOwnership(t *testing.T) {
	const name = "review-group"
	list := fmt.Sprintf(`[{"id":%q,"configuration":{"labels":{"%s":"true","%s":%q,"%s":"ci"}},"status":{"state":"running","networks":[]}}]`,
		name, managedLabel, creationLabel, reuseGroupLabel, reviewCreationA)
	runner := &reviewPruneRunner{
		list:    list,
		inspect: reviewAppleInspect(name, reviewCreationA, "running", ""),
	}
	removed, err := pruneReuseGroupWith(context.Background(), runner, appleEngine{}, "ci")
	if err != nil {
		t.Fatalf("group prune: %v", err)
	}
	if len(removed) != 0 || len(runner.deletes) != 0 {
		t.Fatalf("unowned group was pruned: removed=%v deletes=%v", removed, runner.deletes)
	}
}

type reviewCleanupRunner struct {
	*fakeRunner
	deleteErr error
	deleted   []string
	creation  string
}

func (r *reviewCleanupRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = value
				}
			}
		}
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 125, Stderr: "run failed"}
	}
	if args[0] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		creation := r.creation
		r.mu.Unlock()
		return []byte(strings.ReplaceAll(ownedInspectJSON(args[len(args)-1]), "__CONTAINER_CREATION__", creation)), nil, nil
	}
	if args[0] == "delete" || args[0] == "rm" {
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, r.deleteErr
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReviewFailedCreateJoinsCleanupErrorAndHonorsKeep(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "run failed"}
	deleteErr := &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "delete failed"}
	runner := &reviewCleanupRunner{
		fakeRunner: base,
		deleteErr:  deleteErr,
	}
	runner.inspectJSON = ownedInspectJSON("myctr")
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}))
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if !strings.Contains(err.Error(), runErr.Stderr) || !strings.Contains(err.Error(), deleteErr.Stderr) {
		t.Fatalf("joined error = %v, want both causes", err)
	}

	t.Setenv("CONTAINERGO_KEEP", "1")
	base2 := newTestRunner()
	base2.imagePresent = true
	keepRunner := &reviewCleanupRunner{fakeRunner: base2, deleteErr: deleteErr}
	keepRunner.inspectJSON = ownedInspectJSON("myctr")
	_, err = Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(keepRunner), withEngine(appleEngine{}))
	if err == nil || len(keepRunner.deleted) != 0 {
		t.Fatalf("KEEP cleanup = (%v, %v), want retained failure", err, keepRunner.deleted)
	}
}

type reviewExternalRunner struct {
	*fakeRunner
	creation        string
	sessionOverride string
}

func (r *reviewExternalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = value
				}
			}
		}
		r.mu.Unlock()
		return []byte(strings.Repeat("a", 64) + "\n"), nil, nil
	}
	if args[0] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		creation := r.creation
		session := r.sessionOverride
		r.mu.Unlock()
		if session == "" {
			session = sessionID()
		}
		data := strings.ReplaceAll(ownedInspectJSON(args[len(args)-1]), "__CONTAINER_CREATION__", creation)
		data = strings.ReplaceAll(data, sessionID(), session)
		return []byte(data), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (reviewExternalRunner) External() bool         { return true }
func (reviewExternalRunner) ExternalBinary() string { return "unused" }

func TestReviewReaperRegistrationRequiresOwnership(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := reviewExternalRunner{fakeRunner: base, sessionOverride: "wrong"}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(&runner), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "ownership") && !strings.Contains(err.Error(), "session") {
		t.Fatalf("ownership verification error = %v", err)
	}
	if len(base.calls) == 0 {
		t.Fatal("expected verification inspect")
	}
	for _, args := range base.calls {
		if args[0] == "delete" {
			t.Fatalf("unverified container was deleted: %v", args)
		}
	}
}

type reviewPostCreateRunner struct {
	*fakeRunner
	name         string
	mu           sync.Mutex
	inspectCalls int
	sequence     []string
}

func (r *reviewPostCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		r.inspectCalls++
		n := r.inspectCalls
		r.sequence = append(r.sequence, "inspect:"+fmt.Sprint(n))
		r.mu.Unlock()
		ports := "[]"
		state := "created"
		if n >= 2 {
			state = "running"
			ports = `[{"hostAddress":"127.0.0.1","hostPort":16379,"containerPort":6379,"proto":"tcp"}]`
		}
		name := r.name
		if name == "" {
			name = "postcreate"
		}
		return []byte(reviewAppleInspect(name, reviewCreationA, state, ports)), nil, nil
	}
	if args[0] == "cp" {
		r.mu.Lock()
		r.sequence = append(r.sequence, "copy")
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type reviewHoldingStrategy struct {
	name    string
	held    chan struct{}
	release chan func()
}

func (s *reviewHoldingStrategy) WaitUntilReady(context.Context, wait.Target) error {
	unlock, err := lockName(context.Background(), s.name)
	if err != nil {
		return err
	}
	close(s.held)
	s.release <- unlock
	return nil
}

func TestReviewReuseFinalVerificationBoundsNameLockWait(t *testing.T) {
	oldAttach := reuseAttachTimeout
	oldFinal := reuseFinalVerifyTimeout
	oldPoll := reusePollInterval
	reuseAttachTimeout = 60 * time.Millisecond
	reuseFinalVerifyTimeout = time.Second
	reusePollInterval = time.Millisecond
	defer func() {
		reuseAttachTimeout, reuseFinalVerifyTimeout, reusePollInterval = oldAttach, oldFinal, oldPoll
	}()

	base := newTestRunner()
	base.imagePresent = true
	runner := &reviewPostCreateRunner{fakeRunner: base, name: "final-lock"}
	strategy := &reviewHoldingStrategy{
		name:    "final-lock",
		held:    make(chan struct{}),
		release: make(chan func(), 1),
	}
	// Use a name matching the strategy and a stable running response.
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("final-lock"), WithReuse(), WithWaitStrategy(strategy),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("final reuse lock error = %v, want deadline", err)
	}
	select {
	case unlock := <-strategy.release:
		unlock()
	case <-time.After(time.Second):
		t.Fatal("readiness strategy did not report its held lock")
	}
}

func TestReviewReusePostCreateWaitsForRunningBindings(t *testing.T) {
	oldPoll := reusePollInterval
	oldTimeout := reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = time.Second
	defer func() { reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout }()

	base := newTestRunner()
	base.imagePresent = true
	runner := &reviewPostCreateRunner{fakeRunner: base}
	publish, err := parsePublishSpec("127.0.0.1:16379:6379")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{
		runner:    runner,
		eng:       appleEngine{},
		name:      "postcreate",
		reuse:     true,
		creation:  reviewCreationA,
		published: []publishSpec{publish},
		files:     []File{{HostPath: writeFreshReviewFile(t), ContainerPath: "/tmp/input"}},
	}
	if _, err := reuseCreate(context.Background(), "redis:7-alpine", cfg); err != nil {
		t.Fatalf("reuseCreate: %v", err)
	}
	runner.mu.Lock()
	sequence := append([]string(nil), runner.sequence...)
	runner.mu.Unlock()
	if len(sequence) < 3 || sequence[len(sequence)-1] != "copy" {
		t.Fatalf("post-create sequence = %v, want running/binding poll before copy", sequence)
	}
}

func TestReviewIncompletePlatformMetadataFailsClosed(t *testing.T) {
	data := []byte(`[{"id":"platform","configuration":{"id":"platform","image":{"reference":"redis"},"labels":{},"platform":{"os":"","architecture":"arm64","variant":""}},"status":{"state":"running","networks":[]}}]`)
	info, err := (appleEngine{}).parseInspect(data, "platform")
	if err != nil {
		t.Fatal(err)
	}
	if info.platform != "/arm64/" || !info.platformMeta.osSet {
		t.Fatalf("platform = %q metadata=%+v, want preserved empty OS", info.platform, info.platformMeta)
	}
	if (appleEngine{}).platformCompatible("linux/arm64", info.platform) {
		t.Fatal("empty OS was treated as linux")
	}
	if !(appleEngine{}).platformCompatible("", info.platform) {
		t.Fatal("empty selector was constrained")
	}

	dockerData := []byte(`[{"Id":"` + strings.Repeat("a", 64) + `","Name":"/platform","Platform":"linux","ImageManifestDescriptor":{"platform":{"architecture":"arm64"}},"State":{"Status":"running"}}]`)
	dockerInfo, err := (dockerEngine{}).parseInspect(dockerData, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if dockerInfo.platform != "linux/arm64" {
		t.Fatalf("Docker platform = %q, want descriptor architecture plus retained OS", dockerInfo.platform)
	}
}

func TestReviewGenerationReplacedErrorTextCompatibility(t *testing.T) {
	const want = "container was recreated; refusing to delete replaced container"
	if got := ErrGenerationReplaced.Error(); got != want {
		t.Fatalf("ErrGenerationReplaced = %q, want %q", got, want)
	}
}
