package container

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type recordingStrategy struct {
	called   bool
	endpoint string
	running  bool
	err      error
}

func (s *recordingStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	s.called = true
	if ep, err := target.Endpoint(ctx, "6379/tcp"); err == nil {
		s.endpoint = ep
	}
	if r, err := target.Running(ctx); err == nil {
		s.running = r
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
	if !s.running {
		t.Error("target reports not running")
	}
}

func TestRunWaitFailureRollsBackAndAttachesLogs(t *testing.T) {
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "fatal: config invalid\n"}
	s := &recordingStrategy{err: errors.New("never became ready")}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), WithWaitStrategy(s))
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
	del := f.callWith("delete")
	if del == nil || !slices.Contains(del, "--force") || !slices.Contains(del, "myctr") {
		t.Errorf("rollback delete not issued: %v", f.calls)
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
