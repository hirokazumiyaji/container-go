package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type recordingStrategy struct {
	called   bool
	endpoint string
	state    wait.State
	err      error
}

func (s *recordingStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	s.called = true
	if ep, err := target.Endpoint(ctx, "6379/tcp"); err == nil {
		s.endpoint = ep
	}
	if stateTarget, ok := target.(wait.StateTarget); ok {
		if state, err := stateTarget.State(ctx); err == nil {
			s.state = state
		}
	}
	return s.err
}

func TestRunInvokesWaitStrategyWithAdaptedTarget(t *testing.T) {
	f := newTestRunner()
	s := &recordingStrategy{}

	runTestContainer(t, f, WithExposedPorts("6379/tcp"), WithWaitStrategy(s))

	if !s.called {
		t.Fatal("wait strategy not invoked")
	}
	if s.endpoint != "192.168.64.3:6379" {
		t.Errorf("target endpoint = %q", s.endpoint)
	}
	if s.state != wait.StateRunning {
		t.Errorf("target state = %q, want %q", s.state, wait.StateRunning)
	}
}

func TestRunWaitFailureRollsBackAndAttachesLogs(t *testing.T) {
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "fatal: config invalid\n"}
	s := &recordingStrategy{err: errors.New("never became ready")}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), withEngine(appleEngine{}), WithWaitStrategy(s))
	if err == nil {
		t.Fatal("want error when wait strategy fails")
	}
	if !strings.Contains(err.Error(), "never became ready") {
		t.Errorf("error = %v, want strategy error", err)
	}
	if !strings.Contains(err.Error(), "fatal: config invalid") {
		t.Errorf("error = %v, want log tail attached", err)
	}
	if f.callWith("delete") == nil {
		t.Error("rollback delete not issued")
	}
}

// endpointInspectStrategy forces the wait path through Endpoint so a
// deferred first inspect failure still rolls the container back.
type endpointInspectStrategy struct{}

func (endpointInspectStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	_, err := target.Endpoint(ctx, "6379/tcp")
	return err
}

func TestRunRollsBackWhenWaitEndpointInspectFails(t *testing.T) {
	f := newTestRunner()
	f.failPrefix = "inspect"
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithExposedPorts("6379/tcp"),
		WithWaitStrategy(endpointInspectStrategy{}),
	)
	if err == nil {
		t.Fatal("want error when wait Endpoint inspect fails")
	}
	if !strings.Contains(err.Error(), "failed to become ready") {
		t.Errorf("error = %v, want wait-path failure after deferred inspect", err)
	}
	// Apple has no immutable ID, so rollback fails closed when the
	// generation cannot be verified: no name-based delete, and the
	// leaked container is reported instead of hidden.
	if del := f.callWith("delete"); del != nil {
		t.Errorf("rollback deleted without a verified generation: %v", del)
	}
	if !strings.Contains(err.Error(), "left behind") {
		t.Errorf("error = %v, want the leaked container reported", err)
	}
}

func TestRunRollbackDeletesByImmutableIDWhenInspectFails(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner(), failInspect: true}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(d), withEngine(dockerEngine{}),
		WithExposedPorts("6379/tcp"),
		WithWaitStrategy(endpointInspectStrategy{}),
	)
	if err == nil || strings.Contains(err.Error(), "left behind") {
		t.Fatalf("err = %v, want wait failure with successful rollback", err)
	}
	// docker run printed the container ID; rollback needs no inspect.
	if rm := d.callWith("rm"); rm == nil || rm[len(rm)-1] != dockerFixtureID {
		t.Errorf("rm = %v, want delete by %s", rm, dockerFixtureID)
	}
}

func TestWaitTargetEndpointDefaultsToFirstExposedPort(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("6379/tcp", "8080/tcp"))

	ep, err := waitTarget{c: ctr}.Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep != "192.168.64.3:6379" {
		t.Errorf("endpoint = %q, want first exposed port", ep)
	}
}

func TestWaitTargetEndpointErrorsWithoutExposedPorts(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if _, err := (waitTarget{c: ctr}).Endpoint(context.Background(), ""); err == nil {
		t.Fatal("want error when no ports declared")
	}
}

func TestWaitTargetExecCommand(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, err := waitTarget{c: ctr}.ExecCommand(context.Background(), []string{"true"})
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d", code)
	}
}
