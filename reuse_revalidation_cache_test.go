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

func ownedDockerInspect(uid, generation string, reused, bound bool) string {
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
		"Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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

type mutableInspectRunner struct {
	*fakeRunner
	mu   sync.Mutex
	data string
}

func (r *mutableInspectRunner) setInspect(data string) {
	r.mu.Lock()
	r.data = data
	r.mu.Unlock()
}

func (r *mutableInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		data := r.data
		r.mu.Unlock()
		return []byte(data), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type afterWaitStrategy func()

func (s afterWaitStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s()
	return nil
}

const (
	revalidateTestUIDA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revalidateTestUIDB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revalidateTestGenA = "0123456789abcdef"
	revalidateTestGenB = "fedcba9876543210"
)

func TestReuseRevalidatesAfterReadiness(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	initial := ownedDockerInspect(revalidateTestUIDA, revalidateTestGenA, true, true)
	cases := map[string]struct {
		after  string
		check  func(error) bool
		reason string
	}{
		"identity": {
			after:  ownedDockerInspect(revalidateTestUIDB, revalidateTestGenB, true, true),
			check:  func(err error) bool { return errors.Is(err, ErrGenerationReplaced) },
			reason: "replacement identity",
		},
		"ownership": {
			after:  ownedDockerInspect(revalidateTestUIDA, revalidateTestGenA, false, true),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "WithReuse") },
			reason: "missing ownership",
		},
		"compatibility": {
			after:  ownedDockerInspect(revalidateTestUIDA, revalidateTestGenA, true, false),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "exposed port") },
			reason: "missing published endpoint",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &mutableInspectRunner{fakeRunner: newTestRunner(), data: initial}
			runner.imagePresent = true
			strategy := afterWaitStrategy(func() { runner.setInspect(tc.after) })
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

type replacementInspectRunner struct {
	data string
}

func (r replacementInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(r.data), nil, nil
	}
	return nil, nil, nil
}

type terminalStream struct {
	err    error
	closed chan struct{}
	reads  atomic.Int32
}

func (s *terminalStream) Read([]byte) (int, error) {
	if s.reads.Add(1) == 1 {
		return 0, s.err
	}
	return 0, io.EOF
}
func (s *terminalStream) Close() error          { return nil }
func (s *terminalStream) Done() <-chan struct{} { return s.closed }
func (s *terminalStream) TerminalError() error  { return s.err }

func TestClassifyingStreamCachesOriginalTerminalCLIError(t *testing.T) {
	runner := &fakeRunner{systemUp: false}
	terminal := &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 31, Stderr: "terminal failure"}
	underlying := &terminalStream{err: terminal, closed: make(chan struct{})}
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

func TestReuseRejectsUnmanagedOrUngeneratedAdoption(t *testing.T) {
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

func TestDeleteStoppedReuseRefusesEmptyGeneration(t *testing.T) {
	runner := &replacementInspectRunner{}
	err := deleteStoppedReuse(context.Background(), &config{
		runner: runner, eng: appleEngine{}, name: "shared",
	}, &engineInfo{state: StateStopped, labels: map[string]string{managedLabel: "true", reuseLabel: "true"}})
	if err == nil {
		t.Fatal("deleteStoppedReuse accepted an empty generation")
	}
}

func TestLazyCacheRejectsReplacementNameLookup(t *testing.T) {
	runner := replacementInspectRunner{
		data: ownedDockerInspect("replacement-uid", "replacement-generation", false, true),
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
	ctr.pack().mu.Lock()
	defer ctr.pack().mu.Unlock()
	if ctr.immutableID() != "" || ctr.pack().info != nil {
		t.Fatalf("cached identity = %q, info = %+v; replacement data was pinned", ctr.immutableID(), ctr.pack().info)
	}
}

func TestCacheKeepsIdentityAndEndpointImmutable(t *testing.T) {
	original := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: revalidateTestGenA},
		uid:    revalidateTestUIDA,
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49153}},
	}
	replacement := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: revalidateTestGenA},
		uid:    revalidateTestUIDB,
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49154}},
	}
	ctr := &Container{
		eng:      dockerEngine{},
		creation: revalidateTestGenA,
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
		sync:     &lockPack{uid: revalidateTestUIDA},
	}
	if err := ctr.cacheInfo(original); err != nil {
		t.Fatalf("cache original: %v", err)
	}
	if err := ctr.cacheInfo(replacement); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("cache replacement error = %v, want ErrGenerationReplaced", err)
	}

	ctr.pack().mu.Lock()
	defer ctr.pack().mu.Unlock()
	if ctr.immutableID() != original.uid {
		t.Fatalf("uid = %q, want immutable %q", ctr.immutableID(), original.uid)
	}
	if ctr.pack().info == nil || ctr.pack().info.uid != original.uid {
		t.Fatalf("cached info = %+v, want immutable UID %q", ctr.pack().info, original.uid)
	}
}
