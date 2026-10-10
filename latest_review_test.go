package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type latestExecVerificationRunner struct {
	execErr       error
	inspectStdout string
	inspectErr    error
}

func (r *latestExecVerificationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		return []byte("exec stdout"), []byte("exec stderr"), r.execErr
	case "inspect":
		return []byte(r.inspectStdout), nil, r.inspectErr
	case "system", "version":
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestExecVerificationPreservesInspectFailures(t *testing.T) {
	originalExec := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "query"},
		ExitCode: 7, Stderr: "daemon unavailable",
	}
	cases := []struct {
		name       string
		inspectErr error
		inspectOut string
		wantText   string
		wantAbsent bool
	}{
		{
			name:       "tls",
			inspectErr: errors.New("x509: certificate signed by unknown authority"),
			wantText:   "certificate",
		},
		{
			name:       "permission",
			inspectErr: &cli.CLIError{Binary: "container", Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "permission denied"},
			wantText:   "permission denied",
		},
		{
			name:       "configuration",
			inspectErr: errors.New("invalid configuration for current context"),
			wantText:   "invalid configuration",
		},
		{
			name:       "cancellation",
			inspectErr: context.Canceled,
			wantText:   "context canceled",
		},
		{
			name:       "parse",
			inspectOut: "{",
			wantText:   "decode",
		},
		{
			name:       "empty",
			inspectOut: "",
			wantText:   "empty",
		},
		{
			name:       "successful empty array",
			inspectOut: "[]",
			wantAbsent: true,
		},
		{
			name:       "successful mismatch",
			inspectOut: `[{"id":"other","configuration":{"id":"other","image":{"reference":"redis"},"labels":{}},"status":{"state":"running","networks":[]}}]`,
			wantText:   "inspect target",
			wantAbsent: true,
		},
		{
			name:       "stopped state",
			inspectOut: `[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis"},"labels":{}},"status":{"state":"stopped","networks":[]}}]`,
			wantText:   "state is stopped",
		},
		{
			name: "confirmed absence",
			inspectErr: &cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "Error: container not found: myctr",
			},
			wantAbsent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &latestExecVerificationRunner{
				execErr:       originalExec,
				inspectStdout: tc.inspectOut,
				inspectErr:    tc.inspectErr,
			}
			ctr := &Container{id: "myctr", creation: "0123456789abcdef", runner: r, eng: appleEngine{}}
			_, _, err := ctr.Exec(context.Background(), []string{"query"})
			if err == nil {
				t.Fatal("Exec returned nil error after verification failure")
			}
			if !errors.Is(err, originalExec) {
				t.Fatalf("error = %v, want original exec CLIError", err)
			}
			if tc.inspectErr != nil && !errors.Is(err, tc.inspectErr) {
				t.Fatalf("error = %v, want inspect cause %v", err, tc.inspectErr)
			}
			if tc.wantAbsent != errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("error = %v, ErrContainerNotFound=%t, want %t", err, errors.Is(err, ErrContainerNotFound), tc.wantAbsent)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error = %v, want text %q", err, tc.wantText)
			}
		})
	}
}

func TestJoinedDefinitiveFailureVetoesAbsenceMatchers(t *testing.T) {
	containerAbsence := &cli.CLIError{
		Binary: "container", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: "Error: container not found: myctr",
	}
	imageAbsence := &cli.CLIError{
		Binary: "docker", Args: []string{"image", "inspect", "redis:7-alpine"},
		ExitCode: 1, Stderr: "Error response from daemon: No such image: redis:7-alpine",
	}
	conflictAbsence := &cli.CLIError{
		Binary: "docker", Args: []string{"run", "--name", "myctr"},
		ExitCode: 1, Stderr: `Conflict. The container name "myctr" is already in use by container abc`,
	}
	cases := []struct {
		name string
		eng  engine
		err  error
		want bool
	}{
		{
			name: "container permission",
			eng:  appleEngine{},
			err:  errors.Join(containerAbsence, errors.New("permission denied")),
		},
		{
			name: "image configuration",
			eng:  dockerEngine{},
			err:  errors.Join(imageAbsence, errors.New("invalid configuration for current context")),
		},
		{
			name: "conflict cancellation",
			eng:  dockerEngine{},
			err:  errors.Join(conflictAbsence, errors.New("operation canceled")),
		},
		{
			name: "generic ambiguous joined text",
			eng:  appleEngine{},
			err:  errors.Join(containerAbsence, errors.New("application: no such object: myctr")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isNotFoundFor(tc.eng, tc.err) {
				t.Fatal("joined definitive or ambiguous failure became ErrContainerNotFound")
			}
		})
	}
}

func TestProbePredicateInspectsAllJoinedBranches(t *testing.T) {
	ordinary := &cli.CLIError{
		Binary: "docker", Args: []string{"version"},
		ExitCode: 1, Stderr: "client returned an unrelated status",
	}
	liveness := &cli.CLIError{
		Binary: "docker", Args: []string{"version"},
		ExitCode: 1, Stderr: "Cannot connect to the Docker daemon",
	}
	joined := errors.Join(ordinary, liveness)
	if !dockerProbeUnavailable(joined) {
		t.Fatal("probe predicate ignored a matching liveness CLIError branch")
	}

	config := cli.WithStdout(&cli.CLIError{
		Binary: "docker", Args: []string{"version"}, ExitCode: 1,
	}, "invalid configuration for current context")
	withConfig := errors.Join(liveness, config)
	if dockerProbeUnavailable(withConfig) {
		t.Fatal("probe predicate ignored a joined configuration branch")
	}

	stdoutLiveness := cli.WithStdout(&cli.CLIError{
		Binary: "docker", Args: []string{"version"}, ExitCode: 1,
	}, "Cannot connect to the Docker daemon")
	if !dockerProbeUnavailable(stdoutLiveness) {
		t.Fatal("probe predicate ignored liveness evidence carried on stdout")
	}
	stdoutObject := cli.WithStdout(&cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
	}, "no such object: myctr")
	if (dockerEngine{}).containerMissing(stdoutObject) {
		t.Fatal("stdout application output was treated as stderr object evidence")
	}
}

type latestProbeRunner struct {
	probeErr    error
	probeStdout string
}

func (r *latestProbeRunner) Run(_ context.Context, _ ...string) ([]byte, []byte, error) {
	return []byte(r.probeStdout), nil, r.probeErr
}

func TestPureReturnedProbeTimeoutIsLivenessUnlessJoinedVeto(t *testing.T) {
	original := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"},
		ExitCode: 1, Stderr: "command failed",
	}
	cases := []struct {
		name     string
		probeErr error
		wantDown bool
	}{
		{name: "pure timeout", probeErr: context.DeadlineExceeded, wantDown: true},
		{name: "permission join", probeErr: errors.Join(context.DeadlineExceeded, errors.New("permission denied"))},
		{name: "configuration join", probeErr: errors.Join(context.DeadlineExceeded, errors.New("invalid configuration for current context"))},
		{name: "cancellation join", probeErr: errors.Join(context.DeadlineExceeded, context.Canceled)},
		{name: "pure cancellation", probeErr: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cli.Classify(context.Background(), &latestProbeRunner{probeErr: tc.probeErr}, original, cli.Probe{
				Args: []string{"version"}, Hint: "start the backend",
				IsUnavailable: func(error) bool { return false },
			})
			if errors.Is(got, cli.ErrSystemNotRunning) != tc.wantDown {
				t.Fatalf("error = %v, daemon-down=%t, want %t", got, errors.Is(got, cli.ErrSystemNotRunning), tc.wantDown)
			}
			if !errors.Is(got, original) {
				t.Fatalf("error = %v, want original CLIError", got)
			}
		})
	}
}

func TestTextualCancellationAndAmbiguousNoSuchErrors(t *testing.T) {
	cancelled := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"},
		ExitCode: 1, Stderr: "Error: operation canceled",
	}
	probeErr := &cli.CLIError{
		Binary: "container", Args: []string{"system", "status"},
		ExitCode: 1, Stderr: "XPC connection error",
	}
	got := cli.Classify(context.Background(), &latestProbeRunner{probeErr: probeErr}, cancelled, cli.Probe{
		Args: []string{"system", "status"}, Hint: "start the backend",
		IsUnavailable: func(error) bool { return true },
	})
	if errors.Is(got, cli.ErrSystemNotRunning) {
		t.Fatalf("textual cancellation was classified as daemon down: %v", got)
	}

	ambiguous := []error{
		&cli.CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, Stderr: "Error: application: no such container: myctr"},
		&cli.CLIError{Binary: "docker", Args: []string{"run", "--name", "myctr"}, Stderr: "Error: application: no such object: myctr"},
		&cli.CLIError{Binary: "docker", Args: []string{"pull", "redis:7-alpine"}, Stderr: "Error: application: no such image: redis:7-alpine"},
	}
	for _, err := range ambiguous {
		if !isAmbiguousApplicationError(appleEngine{}, err) && !isAmbiguousApplicationError(dockerEngine{}, err) {
			t.Errorf("isAmbiguousApplicationError(%v) = false, want true", err)
		}
	}
}

func TestImageTargetMatchingIsCaseSensitive(t *testing.T) {
	cases := []struct {
		name string
		eng  engine
		err  error
		want bool
	}{
		{
			name: "docker exact case",
			eng:  dockerEngine{},
			err:  &cli.CLIError{Binary: "docker", Args: []string{"image", "inspect", "redis:ALPINE"}, Stderr: "Error response from daemon: No such image: redis:ALPINE"},
			want: true,
		},
		{
			name: "docker case mismatch",
			eng:  dockerEngine{},
			err:  &cli.CLIError{Binary: "docker", Args: []string{"image", "inspect", "redis:ALPINE"}, Stderr: "Error response from daemon: No such image: redis:alpine"},
		},
		{
			name: "apple exact case",
			eng:  appleEngine{},
			err:  &cli.CLIError{Binary: "container", Args: []string{"image", "inspect", "redis:ALPINE"}, Stderr: "Error: image not found: redis:ALPINE"},
			want: true,
		},
		{
			name: "apple case mismatch",
			eng:  appleEngine{},
			err:  &cli.CLIError{Binary: "container", Args: []string{"image", "inspect", "redis:ALPINE"}, Stderr: "Error: image not found: redis:alpine"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.eng.imageMissing(tc.err); got != tc.want {
				t.Fatalf("imageMissing() = %t, want %t", got, tc.want)
			}
		})
	}
}

type latestInspectRunner struct {
	stdout string
	err    error
}

func (r *latestInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(r.stdout), nil, r.err
	case "system", "version":
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDirectStructuredAbsenceStillMatches(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: "Error: container not found: myctr",
	}
	if !isNotFoundFor(appleEngine{}, err) {
		t.Fatalf("isNotFoundFor(structured absence) = false; ambiguous=%v definitive=%v", isAmbiguousApplicationError(appleEngine{}, err), cli.IsDefinitiveNonLivenessError(err))
	}
	wrapped := fmt.Errorf("%w: %w", ErrContainerNotFound, err)
	if !isNotFoundFor(appleEngine{}, wrapped) {
		t.Fatalf("isNotFoundFor(wrapped structured absence) = false; ambiguous=%v definitive=%v", isAmbiguousApplicationError(appleEngine{}, wrapped), cli.IsDefinitiveNonLivenessError(wrapped))
	}
}

func TestInspectTargetValidationWrapsConfirmedAbsence(t *testing.T) {
	const appleValid = `[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis"},"labels":{}},"status":{"state":"running","networks":[]}}]`
	const appleOther = `[{"id":"other","configuration":{"id":"other","image":{"reference":"redis"},"labels":{}},"status":{"state":"running","networks":[]}}]`
	const dockerValidByName = `[{"Id":"` + dockerFixtureID + `","Name":"/myctr","State":{"Status":"running"},"Config":{"Image":"redis","Labels":{}},"NetworkSettings":{}}]`
	const dockerValidByID = `[{"Id":"` + dockerFixtureID + `","Name":"/other","State":{"Status":"running"},"Config":{"Image":"redis","Labels":{}},"NetworkSettings":{}}]`
	const dockerOther = `[{"Id":"` + dockerFixtureID + `","Name":"/other","State":{"Status":"running"},"Config":{"Image":"redis","Labels":{}},"NetworkSettings":{}}]`
	cases := []struct {
		name       string
		eng        engine
		stdout     string
		uid        string
		wantAbsent bool
		wantOK     bool
		wantParse  bool
	}{
		{name: "apple empty bytes", eng: appleEngine{}, wantAbsent: true},
		{name: "apple empty", eng: appleEngine{}, stdout: "[]", wantAbsent: true},
		{name: "apple mismatch", eng: appleEngine{}, stdout: appleOther, wantAbsent: true},
		{name: "apple valid", eng: appleEngine{}, stdout: appleValid, wantOK: true},
		{name: "apple parse", eng: appleEngine{}, stdout: "{", wantParse: true},
		{name: "docker empty bytes", eng: dockerEngine{}, wantAbsent: true},
		{name: "docker empty", eng: dockerEngine{}, stdout: "[]", wantAbsent: true},
		{name: "docker mismatch", eng: dockerEngine{}, stdout: dockerOther, wantAbsent: true},
		{name: "docker valid name", eng: dockerEngine{}, stdout: dockerValidByName, wantOK: true},
		{name: "docker valid id", eng: dockerEngine{}, stdout: dockerValidByID, uid: dockerFixtureID, wantOK: true},
		{name: "docker parse", eng: dockerEngine{}, stdout: "{", wantParse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctr := &Container{id: "myctr", uid: tc.uid, runner: &latestInspectRunner{stdout: tc.stdout}, eng: tc.eng, nameInspect: true}
			_, err := ctr.State(context.Background())
			switch {
			case tc.wantOK && err != nil:
				t.Fatalf("State error = %v, want nil", err)
			case tc.wantAbsent && !errors.Is(err, ErrContainerNotFound):
				t.Fatalf("State error = %v, want ErrContainerNotFound", err)
			case tc.wantParse && (err == nil || errors.Is(err, ErrContainerNotFound)):
				t.Fatalf("State error = %v, want parse error only", err)
			case !tc.wantOK && !tc.wantAbsent && !tc.wantParse && err == nil:
				t.Fatal("State returned nil error unexpectedly")
			}
		})
	}
}
