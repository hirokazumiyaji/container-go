package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeStub creates an executable shell script and returns its path.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunnerReturnsStdout(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo "out $1"; echo "err" >&2`)}

	stdout, stderr, err := r.Run(context.Background(), "ls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(stdout); got != "out ls\n" {
		t.Errorf("stdout = %q, want %q", got, "out ls\n")
	}
	if got := string(stderr); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
}

func TestExecRunnerNonZeroExitReturnsCLIError(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo "boom" >&2; exit 3`)}

	_, _, err := r.Run(context.Background(), "inspect", "missing")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if cliErr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "boom") {
		t.Errorf("Stderr = %q, want to contain %q", cliErr.Stderr, "boom")
	}
	if !strings.Contains(cliErr.Error(), "inspect") {
		t.Errorf("Error() = %q, want to contain subcommand %q", cliErr.Error(), "inspect")
	}
	if cliErr.Binary == "" || !strings.Contains(cliErr.Error(), cliErr.Binary) {
		t.Errorf("Binary = %q, Error() = %q", cliErr.Binary, cliErr.Error())
	}
}

func TestExecRunnerCapsStderr(t *testing.T) {
	// Emit ~1MiB of stderr, far beyond the 64KiB cap.
	r := &ExecRunner{Binary: writeStub(t, `i=0; while [ $i -lt 16384 ]; do printf '%064d\n' "$i" >&2; i=$((i+1)); done; exit 1`)}

	stdout, stderr, err := r.Run(context.Background(), "run")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	// The returned output buffers stay whole for exec/log results.
	if len(stderr) <= maxStderr {
		t.Errorf("len(returned stderr) = %d, want > %d", len(stderr), maxStderr)
	}
	_ = stdout
}

func TestExecRunnerPreservesLargeSuccessOutput(t *testing.T) {
	// 128 KiB on each stream with exit 0 must come back whole.
	r := &ExecRunner{Binary: writeStub(t, `head -c 131072 /dev/zero; head -c 131072 /dev/zero >&2`)}
	stdout, stderr, err := r.Run(context.Background(), "exec")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stdout) != 131072 {
		t.Errorf("len(stdout) = %d, want 131072", len(stdout))
	}
	if len(stderr) != 131072 {
		t.Errorf("len(stderr) = %d, want 131072", len(stderr))
	}
}

func TestExecRunnerPreservesLargeFailureOutput(t *testing.T) {
	// Non-zero exit still returns whole output; only CLIError is capped.
	r := &ExecRunner{Binary: writeStub(t, `head -c 131072 /dev/zero; head -c 131072 /dev/zero >&2; exit 7`)}
	stdout, stderr, err := r.Run(context.Background(), "exec")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(stdout) != 131072 {
		t.Errorf("len(stdout) = %d, want 131072", len(stdout))
	}
	if len(stderr) != 131072 {
		t.Errorf("len(returned stderr) = %d, want 131072", len(stderr))
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
}

func TestExecRunnerHonorsContextCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `sleep 30`)}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := r.Run(ctx, "logs", "--follow", "x")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %v, want prompt return after cancellation", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want joined *CLIError", err)
	}
	if cliErr.ExitCode == 0 || !slices.Equal(cliErr.Args, []string{"logs", "--follow", "x"}) {
		t.Errorf("CLIError exit/args = %d %v", cliErr.ExitCode, cliErr.Args)
	}
}

func TestCommandRunErrorCancellationRacePreservesExitAndContext(t *testing.T) {
	binary := writeStub(t, `echo cancel-race >&2; exit 7`)
	cmd := exec.Command(binary)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	exitErr := cmd.Run()
	if exitErr == nil {
		t.Fatal("stub command unexpectedly succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := commandRunError(ctx, binary, []string{"exec", "myctr", "query"}, stderr.Bytes(), exitErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want joined *CLIError", err)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
	if cliErr.Binary != binary || !slices.Equal(cliErr.Args, []string{"exec", "myctr", "query"}) {
		t.Errorf("CLIError command = %q %v", cliErr.Binary, cliErr.Args)
	}
	if !strings.Contains(cliErr.Stderr, "cancel-race") {
		t.Errorf("Stderr = %q, want command diagnostic", cliErr.Stderr)
	}
}

func TestCLIErrorIncludesBinaryName(t *testing.T) {
	err := &CLIError{Binary: "docker", Args: []string{"run", "--detach"}, ExitCode: 125, Stderr: "conflict"}
	got := err.Error()
	if !strings.HasPrefix(got, "docker run --detach:") {
		t.Errorf("Error() = %q, want docker prefix", got)
	}
}

func TestCLIErrorDefaultsBinaryToContainer(t *testing.T) {
	err := &CLIError{Args: []string{"inspect", "x"}, ExitCode: 1}
	if !strings.HasPrefix(err.Error(), "container inspect x:") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestExecRunnerDefaultsToContainerBinary(t *testing.T) {
	r := &ExecRunner{}
	if got := r.binary(); got != "container" {
		t.Errorf("binary() = %q, want %q", got, "container")
	}
}

type fakeRunner struct {
	results map[string]fakeResult
}

type fakeResult struct {
	stdout string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	res, ok := f.results[strings.Join(args, " ")]
	if !ok {
		return nil, nil, &CLIError{Args: args, ExitCode: 1, Stderr: "unexpected command"}
	}
	return []byte(res.stdout), nil, res.err
}

var appleProbe = Probe{Args: []string{"system", "status"}, Hint: "run `container system start`"}

func TestClassifyReturnsSystemNotRunningWhenStatusProbeFails(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}},
	}}

	err := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "container system start") {
		t.Errorf("Error() = %q, want hint to run 'container system start'", err.Error())
	}
}

func TestClassifyUsesProbeSpecificHint(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "cannot connect"}
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	r := &fakeRunner{results: map[string]fakeResult{
		strings.Join(probeArgs, " "): {err: &CLIError{Args: probeArgs, ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}},
	}}

	err := Classify(context.Background(), r, orig, Probe{Args: probeArgs, Hint: "start the Docker daemon"})
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "start the Docker daemon") {
		t.Errorf("Error() = %q, want docker hint", err.Error())
	}
}

func TestClassifyKeepsOriginalErrorWhenSystemIsRunning(t *testing.T) {
	orig := &CLIError{Args: []string{"inspect", "x"}, ExitCode: 1, Stderr: "not found"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {stdout: "apiserver is running"},
	}}

	err := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original error preserved", err)
	}
	if errors.Is(err, ErrSystemNotRunning) {
		t.Error("error wrongly classified as ErrSystemNotRunning")
	}
}

func TestClassifyPreservesNonLivenessOriginalAndProbeErrors(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "permission denied: image config rejected"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatal("permission failure was classified as daemon down")
	}
	var gotCLI *CLIError
	if !errors.As(got, &gotCLI) || gotCLI != orig {
		t.Fatalf("errors.As(*CLIError) = %v, want original CLIError", gotCLI)
	}
}

func TestClassifyDoesNotCallAmbiguousMissingDaemonDown(t *testing.T) {
	orig := &CLIError{Args: []string{"run", "--name", "ctr"}, ExitCode: 1, Stderr: "container not found: ctr"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want both errors", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatal("ambiguous missing message was classified as daemon down")
	}
}

func TestClassifyUsesProbePredicateAndPreservesChains(t *testing.T) {
	orig := &CLIError{Args: []string{"inspect", "ctr"}, ExitCode: 1, Stderr: "command failed"}
	probeErr := &CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "transport unavailable"}
	r := &fakeRunner{results: map[string]fakeResult{
		"version": {err: probeErr},
	}}
	probe := Probe{
		Args: []string{"version"}, Hint: "start the daemon",
		IsUnavailable: func(error) bool { return true },
	}

	got := Classify(context.Background(), r, orig, probe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want both errors", got)
	}
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want sentinel from explicit probe predicate", got)
	}
}

func TestClassifyExplicitProbePredicateOverridesBroadOriginalConfigurationText(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "configuration changed while the command was running"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	probe := appleProbe
	probe.IsUnavailable = func(error) bool { return true }

	got := Classify(context.Background(), r, orig, probe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want authoritative probe sentinel", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
	}
}

// CLIError.Error() embeds the argv the caller supplied, so a container or
// image named "tls" would otherwise veto ErrSystemNotRunning. The
// diagnostics that drive that decision must come from the stderr the CLI
// actually wrote.
func TestProbeClassificationIgnoresCallerSuppliedArgv(t *testing.T) {
	for _, name := range []string{"tls", "proxy", "certificate", "x509", "ssh", "config", "credential"} {
		t.Run(name, func(t *testing.T) {
			orig := &CLIError{
				Args: []string{"logs", "-n", "1000", name}, ExitCode: 1,
				Stderr: "XPC connection error: service is not registered",
			}
			if IsProbeConfigurationError(orig) {
				t.Fatalf("target %q was read as a configuration diagnostic", name)
			}
			probeErr := &CLIError{
				Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error",
			}
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}

			got := Classify(context.Background(), r, orig, appleProbe)
			if !errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, want sentinel for target %q", got, name)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
				t.Fatalf("classified error = %v, want original and probe chains", got)
			}
		})
	}
}

// Excluding argv must not exclude real transport diagnostics that reach the
// same predicates through stderr, joined branches, or captured probe output.
func TestProbeConfigurationDetectionSurvivesArgvExclusion(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "stderr",
			err: &CLIError{
				Args: []string{"version"}, ExitCode: 1,
				Stderr: "x509: certificate signed by unknown authority",
			},
		},
		{
			name: "joined branch",
			err: errors.Join(
				&CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "connect failed"},
				errors.New("tls: failed to verify certificate"),
			),
		},
		{
			name: "wrapped non-CLI cause",
			err: fmt.Errorf("probe instrumentation: %w", errors.New("proxyconnect tcp: connection refused")),
		},
		{
			name: "captured probe output",
			err: withProbeOutput(
				&CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "connect failed"},
				nil, []byte("x509: certificate signed by unknown authority"),
			),
		},
		{
			name: "wrapped CLIError",
			err: fmt.Errorf("runner: %w", &CLIError{
				Args: []string{"version"}, ExitCode: 1,
				Stderr: "error during connect: invalid configuration for current context",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsProbeConfigurationError(tc.err) {
				t.Fatalf("IsProbeConfigurationError(%v) = false, want true", tc.err)
			}
			if !isNonLivenessError(tc.err) {
				t.Fatalf("isNonLivenessError(%v) = false, want true", tc.err)
			}
		})
	}
}

// Wrapper-authored text is library or instrumentation output, not caller
// argv, so it stays in the collected diagnostics.
func TestDiagnosticTextKeepsWrapperAndJoinedWording(t *testing.T) {
	cliErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "status failed"}
	text := diagnosticText(fmt.Errorf("runner: %w", errors.Join(
		fmt.Errorf("probe instrumentation: %w", errors.New("cannot connect to backend")),
		cliErr,
	)))
	if !strings.Contains(text, "cannot connect to backend") {
		t.Fatalf("diagnosticText() = %q, want joined liveness wording", text)
	}
	if strings.Contains(text, "system status") {
		t.Fatalf("diagnosticText() = %q, want CLIError argv excluded", text)
	}
}

func TestDefaultProbeUnavailableInspectsCompleteJoinedError(t *testing.T) {
	cliErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "status failed"}
	joined := errors.Join(
		fmt.Errorf("probe instrumentation: %w", errors.New("cannot connect to backend")),
		cliErr,
	)
	wrapped := fmt.Errorf("runner: %w", joined)

	if !defaultProbeUnavailable(wrapped) {
		t.Fatal("wrapped joined liveness evidence was not classified")
	}
	if defaultProbeUnavailable(errors.New("cannot connect to backend")) {
		t.Fatal("plain application error without CLI evidence was classified")
	}
}

func TestClassifyRejectsProbeConfigurationBeforeExplicitPredicate(t *testing.T) {
	diagnostics := []string{
		"error during connect: x509: certificate signed by unknown authority",
		"error during connect: tls: failed to verify certificate",
		"error during connect: proxyconnect tcp: connection refused",
		"error during connect: ssh: handshake failed",
		"error during connect: invalid configuration for current context",
		"error during connect: authentication required",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
			cliProbeErr := &CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: diagnostic}
			probeErr := errors.Join(cliProbeErr, errors.New("runner attached context"))
			r := &fakeRunner{results: map[string]fakeResult{
				"version": {err: probeErr},
			}}
			probe := Probe{
				Args: []string{"version"}, Hint: "start the daemon",
				IsUnavailable: func(error) bool { return true },
			}

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, configuration failure marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, cliProbeErr) {
				t.Fatalf("classified error = %v, want original and probe chains", got)
			}
		})
	}
}

func TestClassifyJoinedProbeConfigurationVetoesJoinedLivenessText(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	cliProbeErr := &CLIError{
		Args: []string{"version"}, ExitCode: 1,
		Stderr: "cannot connect to the docker daemon",
	}
	probeErr := errors.Join(cliProbeErr, errors.New("x509: certificate signed by unknown authority"))
	r := &fakeRunner{results: map[string]fakeResult{
		"version": {err: probeErr},
	}}
	probe := Probe{
		Args: []string{"version"}, Hint: "start the daemon",
		IsUnavailable: func(error) bool { return true },
	}

	got := Classify(context.Background(), r, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, joined configuration branch was ignored", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, cliProbeErr) {
		t.Fatalf("classified error = %v, want original and probe chains", got)
	}
}

func TestClassifyExplicitProbePredicateDoesNotOverrideDefinitiveOriginalFailure(t *testing.T) {
	cases := []string{
		"permission denied while opening the backend endpoint",
		"invalid context: production does not exist",
		"operation canceled by the caller",
	}
	for _, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: stderr}
			probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, definitive original failure marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
				t.Fatalf("classified error = %v, want original and probe errors", got)
			}
		})
	}
}

func TestClassifyPassesThroughNil(t *testing.T) {
	if err := Classify(context.Background(), &fakeRunner{}, nil, appleProbe); err != nil {
		t.Fatalf("Classify(nil) = %v, want nil", err)
	}
}

type hangingProbeRunner struct {
	started chan struct{}
}

func (h *hangingProbeRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	close(h.started)
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func TestClassifyProbeTimesOut(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	start := time.Now()
	err := Classify(context.Background(), r, orig, appleProbe)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Classify took %v, want finite probe timeout", elapsed)
	}
	select {
	case <-r.started:
	default:
		t.Error("probe was not invoked")
	}
}

type definiteErrorAtProbeDeadlineRunner struct {
	err *CLIError
}

func (r *definiteErrorAtProbeDeadlineRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, r.err
}

func TestClassifyDoesNotTreatExpiredProbeContextAsReturnedTimeout(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1,
		Stderr: "permission denied: configuration cannot be loaded",
	}
	start := time.Now()
	got := Classify(context.Background(), &definiteErrorAtProbeDeadlineRunner{err: probeErr}, orig, appleProbe)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Classify took %v, want finite probe timeout", elapsed)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, probe context state raced a definite failure", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and returned probe error", got)
	}
}

func TestClassifyReturnedTimeoutRequiresOnlyTimeoutOrCommandExitCauses(t *testing.T) {
	causes := []struct {
		name  string
		cause error
	}{
		{name: "permission", cause: os.ErrPermission},
		{name: "cancellation", cause: context.Canceled},
		{name: "configuration", cause: errors.New("x509: certificate signed by unknown authority")},
	}
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
			probeErr := errors.Join(context.DeadlineExceeded, tc.cause)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, joined non-timeout cause was ignored", got)
			}
			if !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, tc.cause) {
				t.Fatalf("classified error = %v, want timeout and joined cause", got)
			}
		})
	}

	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := errors.Join(
		context.DeadlineExceeded,
		fmt.Errorf("wrapped probe timeout: %w", context.DeadlineExceeded),
	)
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, pure joined timeout should be liveness", got)
	}
}

func TestClassifyJoinedCLIErrorAndDeadlineUsesTimeoutLiveness(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1, Stderr: "probe command timed out",
	}
	returned := errors.Join(probeErr, context.DeadlineExceeded)
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: returned},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want ErrSystemNotRunning from definitive timeout", got)
	}
	for _, cause := range []error{orig, probeErr, context.DeadlineExceeded} {
		if !errors.Is(got, cause) {
			t.Fatalf("classified error = %v, want chain %v", got, cause)
		}
	}
}

func TestClassifyJoinedCLIErrorAndDeadlineStillVetoesNonLiveness(t *testing.T) {
	diagnostics := []string{
		"permission denied while opening the endpoint",
		"proxy configuration is invalid",
		"tls certificate verification failed",
		"context canceled by the probe",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
			probeErr := &CLIError{
				Args: []string{"system", "status"}, ExitCode: 1, Stderr: diagnostic,
			}
			returned := errors.Join(probeErr, context.DeadlineExceeded)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: returned},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, non-liveness diagnostic marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) || !errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("classified error = %v, want original, probe, and timeout chains", got)
			}
		})
	}
}

func TestClassifyTimeoutTextVetoesConfigurationAndPermission(t *testing.T) {
	diagnostics := []string{
		"configuration load failed",
		"permission denied",
		"proxy connection failed",
		"tls certificate verification failed",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
			probeErr := fmt.Errorf("%s: %w", diagnostic, context.DeadlineExceeded)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, timeout text was ignored", got)
			}
			if !errors.Is(got, context.DeadlineExceeded) || !strings.Contains(got.Error(), diagnostic) {
				t.Fatalf("classified error = %v, want timeout and diagnostic", got)
			}
		})
	}
}

func TestClassifyPreservesCallerCancellationDuringPredicate(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error",
	}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	probe := appleProbe
	probe.IsUnavailable = func(error) bool {
		cancel()
		return true
	}

	got := Classify(ctx, r, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, cancellation during predicate was ignored", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("classified error = %v, want caller cancellation", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
	}
}

func TestClassifyRespectsCallerCancel(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Classify(ctx, r, orig, appleProbe)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original preserved on cancel", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want caller cancellation preserved", err)
	}
	select {
	case <-r.started:
		t.Fatal("probe ran for an already-canceled caller")
	default:
	}
}

type successfulCancelingProbeRunner struct {
	cancel context.CancelFunc
}

func (r *successfulCancelingProbeRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	r.cancel()
	return nil, nil, nil
}

func TestClassifyCallerCancellationAfterSuccessfulProbeWins(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	ctx, cancel := context.WithCancel(context.Background())
	got := Classify(ctx, &successfulCancelingProbeRunner{cancel: cancel}, orig, appleProbe)
	if !errors.Is(got, orig) || !errors.Is(got, context.Canceled) {
		t.Fatalf("error = %v, want original and caller cancellation", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, successful probe was relabeled as daemon down", got)
	}
}

func TestClassifyPreservesOriginalWhenParentCancelsDuringProbe(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-r.started
		cancel()
	}()
	start := time.Now()
	err := Classify(ctx, r, orig, appleProbe)
	if !errors.Is(err, orig) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want original and caller cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Classify took %v, want fast return on parent cancel", elapsed)
	}
}

func TestIsCommandExit(t *testing.T) {
	if !IsCommandExit(&CLIError{Args: []string{"exec"}, ExitCode: 1}) {
		t.Error("CLIError should be a command exit")
	}
	if IsCommandExit(errors.New("executable file not found")) {
		t.Error("launch failure must not count as command exit")
	}
	if IsCommandExit(nil) {
		t.Error("nil must not count as command exit")
	}
}
