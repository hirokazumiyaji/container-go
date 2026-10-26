package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

type waitRunnerFunc func(context.Context, ...string) ([]byte, []byte, error)

func (f waitRunnerFunc) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

func TestWaitTargetBackendStatePolicy(t *testing.T) {
	cases := []struct {
		name   string
		engine engine
		status string
		want   wait.State
	}{
		{name: "Apple created", engine: appleEngine{}, status: "created", want: wait.StateCreated},
		{name: "Apple stopping is terminal", engine: appleEngine{}, status: "stopping", want: wait.StateStopping},
		{name: "Apple unknown", engine: appleEngine{}, status: "future-state", want: wait.StateUnknown},
		{name: "Docker restarting is transient", engine: dockerEngine{}, status: "restarting", want: wait.StateRestarting},
		{name: "Docker paused is terminal", engine: dockerEngine{}, status: "paused", want: wait.StatePaused},
		{name: "Docker unknown", engine: dockerEngine{}, status: "future-state", want: wait.StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validGen := "0123456789abcdef"
			validUID := strings.Repeat("a", 64)
			runner := waitRunnerFunc(func(_ context.Context, _ ...string) ([]byte, []byte, error) {
				if tc.engine.name() == "apple" {
					return []byte(fmt.Sprintf(`[{"id":"myctr","configuration":{"labels":{%q:"true",%q:%q,%q:%q}},"status":{"state":%q}}]`,
						managedLabel, sessionLabel, sessionID(), creationLabel, validGen, tc.status)), nil, nil
				}
				return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":%q}}]`, validUID, tc.status)), nil, nil
			})
			ctr := &Container{id: "myctr", runner: runner, eng: tc.engine}
			if tc.engine.name() == "apple" {
				ctr.creation = validGen
			} else {
				ctr.pack().uid = validUID
			}
			target := waitTarget{c: ctr}

			got, err := target.State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tc.want {
				t.Errorf("State = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitTargetReportsUndeclaredDefaultPort(t *testing.T) {
	target := waitTarget{c: &Container{}}
	_, err := target.Endpoint(context.Background(), "")
	if err == nil {
		t.Fatal("want undeclared-port error")
	}
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestWaitUndeclaredPortErrorRemainsMatchable(t *testing.T) {
	target := waitTarget{c: &Container{}}
	err := wait.ForListeningPort("6379/tcp").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestWaitImplicitPortSelectsOnlyTCP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("53/udp", "80/tcp"))
	target := waitTarget{c: ctr}
	endpoint, err := target.Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "192.168.64.3:80" {
		t.Fatalf("endpoint = %q, want first TCP port", endpoint)
	}
}

func TestWaitImplicitPortPreservesExposedDeclarationOrder(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f,
		WithExposedPorts("80/tcp"),
		WithPublishedPort("127.0.0.1:18081:8081/tcp"),
	)
	endpoint, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "192.168.64.3:80" {
		t.Fatalf("endpoint = %q, want first declared exposed TCP port", endpoint)
	}
}

func TestWaitImplicitPortFallsBackToPublishedTCP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithPublishedPort("127.0.0.1:18081:8081/tcp"))
	endpoint, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "127.0.0.1:18081" {
		t.Fatalf("endpoint = %q, want published-only TCP port", endpoint)
	}
}

func TestWaitImplicitPortRejectsUDPOnlyContainer(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("53/udp"))
	_, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed for UDP-only container", err)
	}
}

func TestRunRejectsUndeclaredWaitPortBeforeImageLookup(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever),
		WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
		withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("Run error = %v, want ErrPortNotExposed", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called before port validation: %v", f.calls)
	}
}

func TestRunValidatesWaitBeforeMissingImage(t *testing.T) {
	tests := []struct {
		name     string
		strategy wait.Strategy
	}{
		{
			name:     "invalid HTTP mutation",
			strategy: wait.ForHTTP("/"),
		},
		{
			name:     "empty exec",
			strategy: wait.ForExec(nil),
		},
		{
			name: "nested invalid",
			strategy: wait.ForAll(
				wait.ForAny(wait.ForListeningPort("not-a-port")),
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			option := WithWaitStrategy(tc.strategy)
			if httpStrategy, ok := tc.strategy.(*wait.HTTPStrategy); ok {
				// Mutate after constructing the option to exercise Run's
				// boundary validation, not only WithWaitStrategy's eager check.
				httpStrategy.WithMethod("GET\n")
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(PullNever), option,
				withRunner(f), withEngine(appleEngine{}))
			if err == nil || !errors.Is(err, wait.ErrInvalidConfiguration) || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("Run error = %v, want ErrInvalidConfiguration", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called before wait validation: %v", f.calls)
			}
		})
	}
}

const procNetHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func procNetLine(port int, state string) string {
	return fmt.Sprintf("   0: 00000000:%04X 00000000:0000 %s 00000000:00000000 00:00000000 00000000 999 0 12345 1 0000000000000000 100 0 0 10 0\n", port, state)
}

func newDockerWaitTarget(runner cli.Runner) waitTarget {
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctr.pack().uid = dockerFixtureID
	return waitTarget{c: ctr}
}

func TestWaitTargetContainerPortStatus(t *testing.T) {
	tests := []struct {
		name string
		port string
		out  string
		code int
		want wait.PortListenStatus
	}{
		{
			name: "IPv4 listening",
			port: "6379/tcp",
			out:  procNetHeader + procNetLine(6379, "0A"),
			want: wait.PortListenOpen,
		},
		{
			name: "IPv6 listening",
			port: "6379",
			out: procNetHeader +
				"   0: 00000000000000000000000000000000:18EB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 999 0 12345 1 0000000000000000 100 0 0 10 0\n",
			want: wait.PortListenOpen,
		},
		{
			name: "established is not listening",
			port: "6379/tcp",
			out:  procNetHeader + procNetLine(6379, "01"),
			want: wait.PortListenClosed,
		},
		{
			name: "different port is not listening",
			port: "6379/tcp",
			out:  procNetHeader + procNetLine(8081, "0A"),
			want: wait.PortListenClosed,
		},
		{
			name: "missing cat reports unsupported",
			port: "6379/tcp",
			code: 127,
			want: wait.PortListenUnsupported,
		},
		{
			name: "missing proc table reports unsupported",
			port: "6379/tcp",
			out:  "cat: /proc/net/tcp: No such file or directory\ncat: /proc/net/tcp6: No such file or directory\n",
			code: 1,
			want: wait.PortListenUnsupported,
		},
		{
			name: "missing tcp6 with readable tcp is still authoritative",
			port: "6379/tcp",
			out:  procNetHeader + procNetLine(6379, "0A"),
			code: 1,
			want: wait.PortListenOpen,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{fakeRunner: newTestRunner(), execStdout: tc.out}
			if tc.code != 0 {
				f.execErr = &cli.CLIError{Binary: "docker", Args: []string{"exec"}, ExitCode: tc.code, Stderr: "cat: not found"}
			}
			target := newDockerWaitTarget(f)
			got, err := target.ContainerPortStatus(context.Background(), tc.port)
			if err != nil {
				t.Fatalf("ContainerPortStatus: %v", err)
			}
			if got != tc.want {
				t.Fatalf("status = %v, want %v", got, tc.want)
			}
			execCall := f.callWith("exec")
			if len(execCall) == 0 {
				t.Fatal("no exec call recorded")
			}
			wantCommand := procNetCommand()
			if len(execCall) < len(wantCommand) {
				t.Fatalf("exec call = %v, want the command %v", execCall, wantCommand)
			}
			if !slices.Equal(execCall[len(execCall)-len(wantCommand):], wantCommand) {
				t.Errorf("exec tail = %v, want %v", execCall, wantCommand)
			}
		})
	}
}

func TestWaitTargetContainerPortStatusUsesDeclaredContainerPort(t *testing.T) {
	// The published host port (18081) is listening, but the declared
	// container port (8081) is not. Checking the host port here would
	// reintroduce exactly the proxy false positive this check prevents.
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: procNetHeader + procNetLine(18081, "0A") + procNetLine(8081, "01"),
	}
	ctr := &Container{
		id:        "myctr",
		runner:    f,
		eng:       dockerEngine{},
		published: []publishSpec{{hostAddr: "127.0.0.1", hostPort: 18081, containerPort: 8081, proto: "tcp", raw: "127.0.0.1:18081:8081/tcp"}},
	}
	ctr.pack().uid = dockerFixtureID
	target := waitTarget{c: ctr}

	status, err := target.ContainerPortStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("ContainerPortStatus: %v", err)
	}
	if status != wait.PortListenClosed {
		t.Fatalf("status = %v, want closed for the declared container port", status)
	}
}

func TestWaitTargetContainerPortStatusSkipsApple(t *testing.T) {
	failIfCalled := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		t.Fatal("Apple Container must not run a container-side listen check")
		return nil, nil, nil
	})
	target := waitTarget{c: &Container{id: "myctr", runner: failIfCalled, eng: appleEngine{}, creation: "0123456789abcdef"}}

	status, err := target.ContainerPortStatus(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("ContainerPortStatus: %v", err)
	}
	if status != wait.PortListenUnsupported {
		t.Fatalf("status = %v, want unsupported", status)
	}
}

func TestWaitTargetContainerPortStatusPropagatesExecError(t *testing.T) {
	execErr := errors.New("temporary exec failure")
	f := &execRunner{fakeRunner: newTestRunner(), execErr: execErr}
	target := newDockerWaitTarget(f)

	status, err := target.ContainerPortStatus(context.Background(), "6379/tcp")
	if !errors.Is(err, execErr) {
		t.Fatalf("error = %v, want %v", err, execErr)
	}
	if status != wait.PortListenUnsupported {
		t.Fatalf("status = %v, want unsupported alongside the error", status)
	}
}

func TestWaitTargetContainerPortStatusFallsBackWhenCheckUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		execErr error
		code    int
		out     string
	}{
		{
			name:    "executable launch error",
			execErr: &exec.Error{Name: "docker", Err: exec.ErrNotFound},
		},
		{
			name:    "backend does not support exec",
			execErr: fmt.Errorf("%w: exec is not available", ErrUnsupportedCapability),
		},
		{
			name: "missing cat exit 127",
			code: 127,
			out:  procNetHeader + procNetLine(6379, "0A"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{fakeRunner: newTestRunner(), execStdout: tc.out, execErr: tc.execErr}
			if tc.code != 0 {
				f.execErr = &cli.CLIError{Binary: "docker", Args: []string{"exec"}, ExitCode: tc.code, Stderr: "cat: not found"}
			}
			target := newDockerWaitTarget(f)
			status, err := target.ContainerPortStatus(context.Background(), "6379/tcp")
			if err != nil {
				t.Fatalf("ContainerPortStatus: %v, want an unavailable check to report unsupported without error", err)
			}
			if status != wait.PortListenUnsupported {
				t.Fatalf("status = %v, want unsupported", status)
			}
		})
	}
}

func TestWaitTargetContainerPortStatusKeepsContextErrorRetryable(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	target := newDockerWaitTarget(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, err := target.ContainerPortStatus(ctx, "6379/tcp")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if status != wait.PortListenUnsupported {
		t.Fatalf("status = %v, want unsupported alongside the error", status)
	}
}

type fixedEndpointWaitTarget struct {
	waitTarget
	endpoint string
}

func (t fixedEndpointWaitTarget) Endpoint(context.Context, string) (string, error) {
	return t.endpoint, nil
}

func (t fixedEndpointWaitTarget) State(context.Context) (wait.State, error) {
	return wait.StateRunning, nil
}

func TestWaitTargetHostOnlyFallbackWhenCatIsMissing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	f := &execRunner{fakeRunner: newTestRunner(), execErr: &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"exec"},
		ExitCode: 127,
		Stderr:   `exec: "cat": executable file not found in $PATH`,
	}}
	target := fixedEndpointWaitTarget{
		waitTarget: newDockerWaitTarget(f),
		endpoint:   ln.Addr().String(),
	}
	err = wait.ForListeningPort("6379/tcp").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v, want the host-only fallback", err)
	}
	if f.callWith("exec") == nil {
		t.Fatal("container-side check was not attempted")
	}
}

func TestWaitTargetClassifiesContainerNotFound(t *testing.T) {
	inspectErr := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `container not found: "myctr"`}
	runner := waitRunnerFunc(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "system" {
			return nil, nil, nil
		}
		return nil, nil, inspectErr
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}}

	_, err := target.State(context.Background())
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want root ErrContainerNotFound", err)
	}
	if !errors.Is(err, wait.ErrTargetNotFound) {
		t.Fatalf("State error = %v, want wait.ErrTargetNotFound", err)
	}
}

func TestWaitTargetReturnsUnknownWithInspectError(t *testing.T) {
	inspectErr := errors.New("temporary inspect failure")
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, nil, inspectErr
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}}

	state, err := target.State(context.Background())
	if state != wait.StateUnknown {
		t.Errorf("State = %q, want %q", state, wait.StateUnknown)
	}
	if !errors.Is(err, inspectErr) {
		t.Errorf("State error = %v, want %v", err, inspectErr)
	}
}

type waitFailureTailRunner struct {
	*dockerRunner
	logTargets []string
}

func (r *waitFailureTailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "logs" {
		target := args[len(args)-1]
		r.mu.Lock()
		r.logTargets = append(r.logTargets, target)
		r.mu.Unlock()
		return []byte("diagnostic tail for " + target + "\n"), nil, nil
	}
	return r.dockerRunner.Run(ctx, args...)
}

func TestWaitFailureLogTailUsesImmutableTarget(t *testing.T) {
	runner := &waitFailureTailRunner{dockerRunner: &dockerRunner{fakeRunner: newTestRunner()}}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(dockerEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("never ready")}),
	)
	if err == nil {
		t.Fatal("want wait failure")
	}

	runner.mu.Lock()
	targets := append([]string(nil), runner.logTargets...)
	runner.mu.Unlock()
	if !slices.Equal(targets, []string{dockerFixtureID}) {
		t.Fatalf("log targets = %v, want immutable ID %s", targets, dockerFixtureID)
	}
	if !strings.Contains(err.Error(), "diagnostic tail for "+dockerFixtureID) {
		t.Errorf("error = %v, want the tail from the immutable target", err)
	}
}

func TestWaitFailureLogTailReturnsNoLogsWithoutVerifiedTarget(t *testing.T) {
	runner := &waitFailureTailRunner{dockerRunner: &dockerRunner{fakeRunner: newTestRunner()}}
	ctr := &Container{id: "replacement", runner: runner, eng: dockerEngine{}}

	tail, err := ctr.logTailWithError(context.Background())
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want ErrGenerationReplaced", err)
	}
	if tail != "" {
		t.Fatalf("tail = %q, want no logs for an unverified target", tail)
	}
	runner.mu.Lock()
	targets := append([]string(nil), runner.logTargets...)
	runner.mu.Unlock()
	if len(targets) != 0 {
		t.Fatalf("log targets = %v, want no log command before verification", targets)
	}
}

func TestWaitTargetHealthReadsLifecycleSnapshot(t *testing.T) {
	validUID := strings.Repeat("a", 64)
	responses := []string{
		`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":{"Status":"starting"}}}]`,
		`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":{"Status":"healthy"}}}]`,
	}
	inspects := 0
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		index := inspects
		if index >= len(responses) {
			index = len(responses) - 1
		}
		inspects++
		return []byte(responses[index]), nil, nil
	})
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctr.pack().uid = validUID
	installDynamicClock(ctr)
	target := waitTarget{c: ctr}

	health, err := target.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health != wait.HealthStarting {
		t.Fatalf("Health = %q, want %q", health, wait.HealthStarting)
	}
	if inspects != 1 {
		t.Fatalf("inspects = %d, want the initial fallback inspect", inspects)
	}

	// The strategy's 1 Hz lifecycle probe is the only caller that refreshes
	// the snapshot; Health itself must not spawn another inspect.
	if _, err := target.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	health, err = target.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health != wait.HealthHealthy {
		t.Fatalf("Health = %q, want the refreshed %q", health, wait.HealthHealthy)
	}
	if inspects != 2 {
		t.Fatalf("inspects = %d, want Health to reuse the lifecycle snapshot", inspects)
	}
}

func TestWaitTargetHealthRejectsAppleBeforeInspect(t *testing.T) {
	inspects := 0
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		inspects++
		return nil, nil, errors.New("Apple healthcheck must not inspect")
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}}

	_, err := target.Health(context.Background())
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
	}
	if !errors.Is(err, wait.ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want wait.ErrInvalidConfiguration", err)
	}
	var configErr *wait.ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *wait.ConfigError", err)
	}
	if inspects != 0 {
		t.Fatalf("inspects = %d, want Apple rejected before the backend runs", inspects)
	}
}

func TestRunRejectsHealthcheckOnAppleBeforeBackendCalls(t *testing.T) {
	strategies := []wait.Strategy{
		wait.ForHealthcheck(),
		wait.ForAll(wait.ForExec([]string{"true"}), wait.ForHealthcheck()),
	}
	for _, strategy := range strategies {
		f := newTestRunner()
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"),
			WithWaitStrategy(strategy),
			withRunner(f), withEngine(appleEngine{}))
		if !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatalf("Run error = %v, want ErrUnsupportedCapability", err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("backend was called before the Apple healthcheck rejection: %v", f.calls)
		}
	}
}

func TestWaitTargetMissingHealthcheckIsConfigurationError(t *testing.T) {
	validUID := strings.Repeat("a", 64)
	inspects := 0
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		inspects++
		return []byte(`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":null}}]`), nil, nil
	})
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctr.pack().uid = validUID
	installDynamicClock(ctr)
	target := waitTarget{c: ctr}

	err := wait.ForHealthcheck().
		WithStartupTimeout(time.Hour).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, wait.ErrInvalidConfiguration) {
		t.Fatalf("WaitUntilReady error = %v, want wait.ErrInvalidConfiguration", err)
	}
	var configErr *wait.ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *wait.ConfigError", err)
	}
	if inspects != 1 {
		t.Errorf("inspects = %d, want the missing HEALTHCHECK to fail from the lifecycle snapshot", inspects)
	}
}

func TestWaitTargetHealthReinspectingStaleSnapshot(t *testing.T) {
	validUID := strings.Repeat("a", 64)
	inspects := 0
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		inspects++
		return []byte(`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":{"Status":"unhealthy"}}}]`), nil, nil
	})
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctr.pack().uid = validUID
	clock := installDynamicClock(ctr)
	ctr.storeDynamic(&engineInfo{
		state:      StateRunning,
		health:     "healthy",
		observedAt: clock.Now(),
	}, ctr.dynamicGeneration())
	target := waitTarget{c: ctr}

	// A snapshot within the lifecycle probe interval is reused.
	health, err := target.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health != wait.HealthHealthy {
		t.Fatalf("Health = %q, want the cached %q", health, wait.HealthHealthy)
	}
	if inspects != 0 {
		t.Fatalf("inspects = %d, want cached health within the interval", inspects)
	}

	// Once the snapshot is older than the interval, the cached healthy
	// result must not hide a later unhealthy observation.
	clock.Advance(dynamicInspectTTL + time.Millisecond)
	health, err = target.Health(context.Background())
	if err != nil {
		t.Fatalf("Health after stale snapshot: %v", err)
	}
	if health != wait.HealthUnhealthy {
		t.Fatalf("Health = %q, want the fresh %q", health, wait.HealthUnhealthy)
	}
	if inspects != 1 {
		t.Fatalf("inspects = %d, want one fresh inspect for the stale snapshot", inspects)
	}

	// The fresh result becomes the new cached observation.
	health, err = target.Health(context.Background())
	if err != nil {
		t.Fatalf("Health after fresh inspect: %v", err)
	}
	if health != wait.HealthUnhealthy {
		t.Fatalf("Health = %q, want the refreshed %q", health, wait.HealthUnhealthy)
	}
	if inspects != 1 {
		t.Fatalf("inspects = %d, want the fresh result to be reused within the interval", inspects)
	}
}

func TestWaitTargetHealthFreshInspectUsesQueryTimeout(t *testing.T) {
	validUID := strings.Repeat("a", 64)
	queryTimeout := 25 * time.Millisecond
	runner := waitRunnerFunc(func(ctx context.Context, _ ...string) ([]byte, []byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("fresh health inspect has no deadline")
		} else if remaining := time.Until(deadline); remaining > queryTimeout+10*time.Millisecond {
			t.Errorf("inspect deadline is %v away, want at most the %v query timeout", remaining, queryTimeout)
		}
		return []byte(`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":{"Status":"healthy"}}}]`), nil, nil
	})
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}, queryTimeout: queryTimeout}
	ctr.pack().uid = validUID

	// The caller's deadline is much longer than the query timeout. The
	// fallback health inspect must use the shorter query budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	health, err := (waitTarget{c: ctr}).Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health != wait.HealthHealthy {
		t.Fatalf("Health = %q, want %q", health, wait.HealthHealthy)
	}
}

func TestWaitTargetHealthInspectRateStaysAtLifecycleCadence(t *testing.T) {
	validUID := strings.Repeat("a", 64)
	inspects := 0
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		inspects++
		return []byte(`[{"Id":"` + validUID + `","Name":"/myctr","State":{"Status":"running","Health":{"Status":"starting"}}}]`), nil, nil
	})
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctr.pack().uid = validUID
	target := waitTarget{c: ctr}

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	err := wait.ForHealthcheck().
		WithPollInterval(100*time.Millisecond).
		WithStartupTimeout(5*time.Second).
		WaitUntilReady(ctx, target)
	if err == nil {
		t.Fatal("want a timeout while the healthcheck stays starting")
	}
	if inspects > 2 {
		t.Fatalf("inspects = %d over 1.2s, want at most one per lifecycle interval", inspects)
	}
}
