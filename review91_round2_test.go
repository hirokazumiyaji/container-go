package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func review91OwnedDockerInspect(uid, generation string, reused, bound bool) string {
	ports := `{}`
	if bound {
		ports = `{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}`
	}
	reuseLabelJSON := ""
	if reused {
		reuseLabelJSON = fmt.Sprintf(`,"%s":"true"`, reuseLabel)
	}
	return fmt.Sprintf(`[{
		"Id":%q,
		"Name":"/myctr",
		"Config":{"Image":"redis:7-alpine","Labels":{
			"%s":"true",
			"%s":%q,
			"%s":%q%s
		}},
		"State":{"Status":"running"},
		"HostConfig":{"NetworkMode":"bridge"},
		"NetworkSettings":{"IPAddress":"172.17.0.2","Ports":%s,"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}
	}]`, uid, managedLabel, sessionLabel, sessionID(), creationLabel, generation, reuseLabelJSON, ports)
}

type review91MutableInspectRunner struct {
	*fakeRunner
	mu   sync.Mutex
	data string
}

func (r *review91MutableInspectRunner) setInspect(data string) {
	r.mu.Lock()
	r.data = data
	r.mu.Unlock()
}

func (r *review91MutableInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		data := r.data
		r.mu.Unlock()
		return []byte(data), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type review91AfterWaitStrategy func()

func (s review91AfterWaitStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s()
	return nil
}

const (
	review91Round2UIDA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	review91Round2UIDB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	review91Round2GenA = "0123456789abcdef"
	review91Round2GenB = "fedcba9876543210"
)

func TestReview91ReuseRevalidatesAfterReadiness(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	initial := review91OwnedDockerInspect(review91Round2UIDA, review91Round2GenA, true, true)
	cases := map[string]struct {
		after  string
		check  func(error) bool
		reason string
	}{
		"identity": {
			after:  review91OwnedDockerInspect(review91Round2UIDB, review91Round2GenB, true, true),
			check:  func(err error) bool { return errors.Is(err, ErrGenerationReplaced) },
			reason: "replacement identity",
		},
		"ownership": {
			after:  review91OwnedDockerInspect(review91Round2UIDA, review91Round2GenA, false, true),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "WithReuse") },
			reason: "missing ownership",
		},
		"compatibility": {
			after:  review91OwnedDockerInspect(review91Round2UIDA, review91Round2GenA, true, false),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "exposed port") },
			reason: "missing published endpoint",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &review91MutableInspectRunner{fakeRunner: newTestRunner(), data: initial}
			runner.imagePresent = true
			strategy := review91AfterWaitStrategy(func() { runner.setInspect(tc.after) })
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithReuse(), WithExposedPorts("6379/tcp"),
				WithWaitStrategy(strategy), withRunner(runner), withEngine(dockerEngine{}))
			if err == nil || !tc.check(err) {
				if ctr != nil {
					_ = ctr.Terminate(context.Background())
				}
				t.Fatalf("Run returned container %v with error %v, want %s failure", ctr, err, tc.reason)
			}
		})
	}
}

type review91ReplacementInspectRunner struct {
	data string
}

func (r review91ReplacementInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(r.data), nil, nil
	}
	return nil, nil, nil
}

type review91TerminalStream struct {
	err    error
	closed chan struct{}
	reads  atomic.Int32
}

func (s *review91TerminalStream) Read([]byte) (int, error) {
	if s.reads.Add(1) == 1 {
		return 0, s.err
	}
	return 0, io.EOF
}
func (s *review91TerminalStream) Close() error          { return nil }
func (s *review91TerminalStream) Done() <-chan struct{} { return s.closed }
func (s *review91TerminalStream) TerminalError() error  { return s.err }

func TestReview91ClassifyingStreamCachesOriginalTerminalCLIError(t *testing.T) {
	runner := &fakeRunner{systemUp: false}
	terminal := &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 31, Stderr: "terminal failure"}
	underlying := &review91TerminalStream{err: terminal, closed: make(chan struct{})}
	stream := &classifyingStream{ReadCloser: underlying, ctx: context.Background(), container: &Container{
		id: "myctr", runner: runner, eng: appleEngine{},
	}}

	_, err := stream.Read(make([]byte, 1))
	var got *cli.CLIError
	if !errors.As(err, &got) || got.ExitCode != terminal.ExitCode {
		t.Fatalf("Read error = %v, want original CLIError", err)
	}
	if terminalErr := stream.TerminalError(); !errors.As(terminalErr, &got) || got.ExitCode != terminal.ExitCode {
		t.Fatalf("TerminalError = %v, want cached original CLIError", terminalErr)
	}
	runner.mu.Lock()
	probeCalls := 0
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "system" {
			probeCalls++
		}
	}
	runner.mu.Unlock()
	if probeCalls != 1 {
		t.Fatalf("system probe calls = %d, want one cached classification", probeCalls)
	}
}

func TestReview91ReuseRejectsUnmanagedOrUngeneratedAdoption(t *testing.T) {
	cases := map[string]map[string]string{
		"unmanaged":   {reuseLabel: "true", creationLabel: "aaaaaaaaaaaaaaaa"},
		"ungenerated": {managedLabel: "true", reuseLabel: "true"},
	}
	for name, labels := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkReuseOwned(&engineInfo{labels: labels}, "redis:7-alpine", &config{name: "shared"})
			if err == nil {
				t.Fatal("checkReuseOwned accepted an unverifiable adoption")
			}
		})
	}
}

func TestReview91DeleteStoppedReuseRefusesEmptyGeneration(t *testing.T) {
	runner := &review91ReplacementInspectRunner{}
	err := deleteStoppedReuse(context.Background(), &config{
		runner: runner, eng: appleEngine{}, name: "shared",
	}, &engineInfo{state: StateStopped, labels: map[string]string{managedLabel: "true", reuseLabel: "true"}})
	if err == nil {
		t.Fatal("deleteStoppedReuse accepted an empty generation")
	}
}

func TestReview91LazyCacheRejectsReplacementNameLookup(t *testing.T) {
	runner := review91ReplacementInspectRunner{
		data: review91OwnedDockerInspect("replacement-uid", "replacement-generation", false, true),
	}
	ctr := &Container{
		id:       "myctr",
		runner:   runner,
		eng:      dockerEngine{},
		creation: "original-generation",
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
	}

	info, err := ctr.cachedInfo(context.Background())
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("info = %+v, error = %v, want ErrGenerationReplaced", info, err)
	}
	ctr.mu.Lock()
	defer ctr.mu.Unlock()
	if ctr.uid != "" || ctr.info != nil {
		t.Fatalf("cached identity = %q, info = %+v; replacement data was pinned", ctr.uid, ctr.info)
	}
}

func TestReview91CacheKeepsIdentityAndEndpointImmutable(t *testing.T) {
	original := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: review91Round2GenA},
		uid:    review91Round2UIDA,
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49153}},
	}
	replacement := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: review91Round2GenA},
		uid:    review91Round2UIDB,
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49154}},
	}
	ctr := &Container{
		eng:      dockerEngine{},
		creation: review91Round2GenA,
		uid:      review91Round2UIDA,
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
	}
	if err := ctr.cacheInfo(original); err != nil {
		t.Fatalf("cache original: %v", err)
	}
	if err := ctr.cacheInfo(replacement); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("cache replacement error = %v, want ErrGenerationReplaced", err)
	}

	ctr.mu.Lock()
	defer ctr.mu.Unlock()
	if ctr.uid != original.uid {
		t.Fatalf("uid = %q, want immutable %q", ctr.uid, original.uid)
	}
	if ctr.info == nil || ctr.info.uid != original.uid {
		t.Fatalf("cached info = %+v, want immutable UID %q", ctr.info, original.uid)
	}
}
