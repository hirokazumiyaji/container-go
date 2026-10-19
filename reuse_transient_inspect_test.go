package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type reuseTransientRunner struct {
	*fakeRunner
	mu       sync.Mutex
	steps    []reuseTransientInspectStep
	inspects int
}

type reuseTransientInspectStep struct {
	data string
	err  error
}

const reuseTransientDockerUID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (r *reuseTransientRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		i := r.inspects
		if i >= len(r.steps) {
			i = len(r.steps) - 1
		} else {
			r.inspects++
		}
		step := r.steps[i]
		r.mu.Unlock()
		return []byte(step.data), nil, step.err
	}
	if args[0] == "run" {
		return []byte(reuseTransientDockerUID + "\n"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *reuseTransientRunner) inspectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inspects
}

func reuseTransientInspect(bound bool) string {
	return reuseTransientInspectIdentity(bound, reuseTransientDockerUID, "aaaaaaaaaaaaaaaa")
}

func reuseTransientInspectIdentity(bound bool, uid, creation string) string {
	ports := `{}`
	if bound {
		ports = `{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}`
	}
	creationLabel := ""
	if creation != "" {
		creationLabel = fmt.Sprintf(`,"com.github.hirokazumiyaji.container-go.creation":%q`, creation)
	}
	sessionLabelJSON := fmt.Sprintf(`,"com.github.hirokazumiyaji.container-go.session":%q`, sessionID())
	return fmt.Sprintf(`[{
		"Id":%q,
		"Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"Name":"/myctr",
		"Config":{"Image":"redis:7-alpine","Labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.reuse":"true"%s%s}},
		"State":{"Status":"running"},
		"HostConfig":{"NetworkMode":"bridge"},
		"NetworkSettings":{"IPAddress":"172.17.0.2","Ports":%s,"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}
	}]`, uid, sessionLabelJSON, creationLabel, ports)
}

func TestWithReuseRetriesTransientInspect(t *testing.T) {
	oldPoll := reusePollInterval
	oldTimeout := reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval = oldPoll
		reuseAttachTimeout = oldTimeout
	})

	runner := &reuseTransientRunner{
		fakeRunner: newTestRunner(),
		steps: []reuseTransientInspectStep{
			{err: errors.New("temporary inspect failure")},
			{data: reuseTransientInspect(true)},
		},
	}
	runner.imagePresent = true

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithExposedPorts("6379/tcp"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %+v, want attached handle", ctr)
	}
	if got := runner.inspectCount(); got < 2 {
		t.Fatalf("inspect calls = %d, want retry after transient failure", got)
	}
}

func TestWithReuseRetriesIncompleteInspect(t *testing.T) {
	oldPoll := reusePollInterval
	oldTimeout := reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval = oldPoll
		reuseAttachTimeout = oldTimeout
	})

	runner := &reuseTransientRunner{
		fakeRunner: newTestRunner(),
		steps: []reuseTransientInspectStep{
			{data: reuseTransientInspect(false)},
			{data: reuseTransientInspect(false)},
			{data: reuseTransientInspect(true)},
		},
	}
	runner.imagePresent = true

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithExposedPorts("6379/tcp"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.inspectCount(); got < 3 {
		t.Fatalf("inspect calls = %d, want incomplete inspects to be retried", got)
	}
}

type createInspectRunner struct {
	*fakeRunner
	created  bool
	inspects int
	creation string
}

func (r *createInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.created {
			return nil, nil, &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: `Error: No such container: myctr`}
		}
		r.inspects++
		if r.inspects == 1 {
			return nil, nil, errors.New("temporary post-create inspect failure")
		}
		return []byte(reuseTransientInspectIdentity(true, reuseTransientDockerUID, r.creation)), nil, nil
	case "run":
		r.created = true
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = creation
				}
			}
		}
		return []byte(reuseTransientDockerUID + "\n"), nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestWithReuseRetriesInspectAfterCreate(t *testing.T) {
	oldPoll := reusePollInterval
	oldTimeout := reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval = oldPoll
		reuseAttachTimeout = oldTimeout
	})

	runner := &createInspectRunner{fakeRunner: newTestRunner()}
	runner.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithExposedPorts("6379/tcp"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %+v, want created handle", ctr)
	}
	runner.mu.Lock()
	inspects := runner.inspects
	runner.mu.Unlock()
	if inspects < 2 {
		t.Fatalf("post-create inspects = %d, want transient retry", inspects)
	}
}

func TestReuseTransientErrorIsNotClassifiedAsMissing(t *testing.T) {
	// Keep this assertion close to the runner: a plain inspect error must
	// remain retryable rather than being mistaken for a not-found result.
	err := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "daemon temporarily unavailable"}
	if isNotFound(err) {
		t.Fatalf("isNotFound(%v) = true, want false", err)
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("error = %v, want original diagnostic", err)
	}
}
