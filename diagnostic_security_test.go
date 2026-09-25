package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestRunCLIErrorUsesRedactedArgsAndStderr(t *testing.T) {
	const (
		envSecret    = "run-env-password"
		commandToken = "run-command-token"
		labelSecret  = "run-label-password"
		stderrToken  = "run-stderr-token"
	)
	f := newTestRunner()
	f.failPrefix = "run"
	f.failStderr = "password=" + envSecret + " token=" + stderrToken + "\x1b[31m\r\n"

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithEnv(map[string]string{"PASSWORD": envSecret}),
		WithCmd("server", commandToken),
		WithLabels(map[string]string{"com.example.password": labelSecret}),
	)
	if err == nil {
		t.Fatal("want CLI error")
	}
	got := err.Error()
	for _, secret := range []string{envSecret, commandToken, labelSecret, stderrToken} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains secret %q", got, secret)
		}
	}
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("error = %q, contains terminal control characters", got)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(cliErr.Error(), commandToken) {
		t.Errorf("errors.As CLIError.Error() = %q, contains command secret", cliErr.Error())
	}
	if !strings.Contains(cliErr.RawError(), commandToken) {
		t.Errorf("RawError() = %q, want explicit raw command", cliErr.RawError())
	}
}

func TestRunWaitDiagnosticsRedactCallerSecretsAndControlCharacters(t *testing.T) {
	const (
		envSecret    = "env-password-value"
		commandToken = "cmd-token-value"
		labelSecret  = "label-secret-value"
		stderrSecret = "stderr-secret-value"
	)
	f := newTestRunner()
	logs := &stdoutRunner{
		fakeRunner: f,
		stdout: "starting\n" +
			"PASSWORD=" + envSecret + "\n" +
			"token=" + commandToken + "\x1b[31mred\x1b[0m\r\n" +
			"stderr=" + stderrSecret + "\n" +
			"label=" + labelSecret,
	}
	strategy := &recordingStrategy{err: errors.New("not ready\x1b[2J\r\n")}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), withEngine(appleEngine{}),
		WithEnv(map[string]string{"PASSWORD": envSecret}),
		WithCmd("server", commandToken),
		WithLabels(map[string]string{"com.example.token": labelSecret}),
		WithWaitStrategy(strategy),
	)
	if err == nil {
		t.Fatal("want wait error")
	}
	got := err.Error()
	for _, secret := range []string{envSecret, commandToken, labelSecret, stderrSecret} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains secret %q", got, secret)
		}
	}
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("error = %q, contains terminal control characters", got)
	}
	if !strings.Contains(got, "container logs") {
		t.Errorf("error = %q, want redacted log tail", got)
	}
}

func TestRunRejectsOptionValuesWithoutEchoingThem(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		option Option
	}{
		{"name", "rejected-name-secret", WithName("rejected-name-secret!")},
		{"environment value", "rejected-env-secret", WithEnv(map[string]string{"PASSWORD": "rejected-env-secret\n"})},
		{"memory", "rejected-memory-secret", WithMemory("rejected-memory-secret")},
		{"published port", "rejected-port-secret", WithPublishedPort("rejected-port-secret")},
		{"file path", "rejected-file-secret", WithFiles(File{ContainerPath: "rejected-file-secret"})},
		{"pull policy", "99", WithPullPolicy(PullPolicy(99))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine", tc.option, withRunner(f), withEngine(appleEngine{}))
			if err == nil {
				t.Fatal("want validation error")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error = %q, echoes rejected value %q", err, tc.secret)
			}
			var validation *ValidationError
			if !errors.As(err, &validation) || !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %T %v, want typed validation error", err, err)
			}
		})
	}
}

func TestRunRejectsEnvValueBeyondStreamSafetyLimit(t *testing.T) {
	secret := strings.Repeat("s", diagnostic.MaxStreamOverlap+1)
	_, err := Run(context.Background(), "redis:7-alpine", WithEnv(map[string]string{"TOKEN": secret}), withRunner(newTestRunner()))
	if err == nil {
		t.Fatal("want validation error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("validation error echoes oversized value")
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestRunCustomOptionErrorIsValueFreeAtPublicBoundary(t *testing.T) {
	const secret = "custom-option-secret-117"
	badOption := Option(func(*config) error { return errors.New("rejected " + secret) })
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine", badOption, withRunner(f), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want option error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains custom option value", err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestRunWaitDiagnosticsCarryHTTPHeaderBasicAuthPatternExecAndPort(t *testing.T) {
	const (
		header   = "wait-header-secret-117"
		password = "wait-password-secret-117"
		pattern  = "wait-pattern-secret-117"
		command  = "wait-command-secret-117"
		port     = "6553/tcp"
	)
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "header=" + header + " password=" + password + " pattern=" + pattern + " command=" + command + " port=" + port + "\n"}
	strategy := wait.ForAll(
		wait.ForHTTP("/private-health").WithPort("8080/tcp").WithHeader("X-Private", header).WithBasicAuth("user", password).WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond),
		wait.ForLog(pattern).WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond),
		wait.ForExec([]string{"tool", command}).WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond),
		wait.ForListeningPort(port).WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond),
	)
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("diag-wait-117"), withRunner(logs), withEngine(appleEngine{}), WithWaitStrategy(strategy))
	if err == nil {
		t.Fatal("want wait error")
	}
	got := err.Error()
	for _, secret := range []string{header, password, pattern, command, port} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains wait context %q", got, secret)
		}
	}
	if !strings.Contains(got, "container logs") {
		t.Errorf("error = %q, want log tail", got)
	}
}

func TestReuseWaitDiagnosticsCarryStrategyContext(t *testing.T) {
	const secret = "reuse-wait-secret-117"
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "pattern=" + secret + "\n"}
	strategy := wait.ForLog(secret).WithStartupTimeout(20 * time.Millisecond).WithPollInterval(time.Millisecond)
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("diag-reuse-117"), WithReuse(), withRunner(logs), withEngine(appleEngine{}), WithWaitStrategy(strategy))
	if err == nil {
		t.Fatal("want reuse wait error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("reuse error = %q, contains wait secret", err)
	}
}

func TestLogTailRedactsBeforeFinalTruncation(t *testing.T) {
	const secret = "tail-boundary-secret-117"
	data := strings.Repeat("a", logTailLimit-4) + secret + strings.Repeat("b", 128)
	runner := &fixedTailRunner{data: []byte(data)}
	ctr := &Container{id: "diag-tail", runner: runner, eng: appleEngine{}, diagnosticSecrets: []string{secret}}
	got := ctr.logTail(context.Background())
	if len(got) > logTailLimit {
		t.Fatalf("tail length = %d, want <= %d", len(got), logTailLimit)
	}
	if strings.Contains(got, secret) || strings.Contains(got, secret[4:]) {
		t.Fatalf("tail = %q, contains a split secret", got)
	}
}

func TestContainerUsesBoundedHashedDiagnosticMatcher(t *testing.T) {
	const secret = "handle-lifetime-secret-117"
	ctr := runTestContainer(t, newTestRunner(), WithEnv(map[string]string{"TOKEN": secret}))
	if len(ctr.diagnosticSecrets) != 0 || ctr.diagnosticRedactorValue == nil {
		t.Fatalf("handle retained plaintext diagnostics: secrets=%v matcher=%v", ctr.diagnosticSecrets, ctr.diagnosticRedactorValue != nil)
	}
	if strings.Contains(ctr.diagnosticRedactor().Text(secret), secret) {
		t.Fatal("hashed handle matcher failed to redact its value")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if ctr.diagnosticRedactorValue != nil || len(ctr.diagnosticSecrets) != 0 {
		t.Fatal("Terminate did not clear diagnostic matcher references")
	}
}

func TestRunSystemNotRunningRetainsTypedChains(t *testing.T) {
	f := &classifiedRunner{fakeRunner: newTestRunner()}
	_, err := Run(context.Background(), "redis:7-alpine", WithName("diag-system-117"), withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	var system *SystemNotRunningError
	if !errors.As(err, &system) || system.OriginalError() == nil || system.ProbeError() == nil {
		t.Fatalf("error = %T %v, want original and probe chains", err, err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatal("errors.As did not find original CLIError")
	}
	if strings.Contains(cliErr.Error(), "original-secret") || strings.Contains(cliErr.Stderr, "original-secret") {
		t.Fatalf("errors.As CLIError is not redacted: %q", cliErr.Error())
	}
	if !strings.Contains(cliErr.RawError(), "original-secret") {
		t.Fatalf("RawError() = %q, want raw original", cliErr.RawError())
	}
}

type classifiedRunner struct{ *fakeRunner }

func (r *classifiedRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case "run":
		return nil, nil, &cli.CLIError{Args: args, Stderr: "original-secret", ExitCode: 1}
	case "system":
		return nil, nil, &cli.CLIError{Args: args, Stderr: "probe-secret", ExitCode: 1}
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

type fixedTailRunner struct{ data []byte }

func (r *fixedTailRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	return r.data, nil, nil
}
