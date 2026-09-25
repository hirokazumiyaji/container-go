package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// execRunner extends fakeRunner with canned exec results.
type execRunner struct {
	*fakeRunner
	execStdout string
	execErr    error
}

func (e *execRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		e.calls = append(e.calls, args)
		for i, a := range args {
			if a == "--env-file" && i+1 < len(args) {
				data, _ := os.ReadFile(args[i+1])
				e.envFiles = append(e.envFiles, string(data))
			}
		}
		stderr := []byte("stderr-part")
		var cliErr *cli.CLIError
		if errors.As(e.execErr, &cliErr) {
			stderr = []byte(cliErr.Stderr)
		}
		return []byte(e.execStdout), stderr, e.execErr
	}
	return e.fakeRunner.Run(ctx, args...)
}

func TestExecReturnsZeroExitAndOutput(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "hello\n"}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "hello\n") {
		t.Errorf("output = %q", data)
	}

	execCall := f.callWith("exec")
	i := slices.Index(execCall, "myctr")
	if i < 0 || !slices.Equal(execCall[i+1:], []string{"echo", "hello"}) {
		t.Errorf("exec args = %v", execCall)
	}
}

func TestExecReturnsCommandExitCodeWithoutError(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "process failed"},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "process failed" {
		t.Errorf("output = %q, want %q", string(data), "process failed")
	}
}

func TestExecReportsMissingContainerAsError(t *testing.T) {
	f := &execMissingRunner{
		execRunner: &execRunner{
			fakeRunner: newTestRunner(),
			execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
	}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err == nil {
		t.Fatal("want error for missing container")
	}
}

type execMissingRunner struct {
	*execRunner
}

func (m *execMissingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
	}
	return m.execRunner.Run(ctx, args...)
}

type execInspectFailureRunner struct {
	*execRunner
	inspectStdout []byte
	inspectErr    error
}

func (r *execInspectFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		return r.inspectStdout, nil, r.inspectErr
	}
	return r.execRunner.Run(ctx, args...)
}

func TestExecPropagatesInspectVerificationErrors(t *testing.T) {
	cases := []struct {
		name            string
		inspectStdout   []byte
		inspectErr      error
		wantNotFound    bool
		wantSyntaxError bool
		wantSchemaError bool
	}{
		{
			name: "target missing",
			inspectErr: &cli.CLIError{
				Args:   []string{"inspect", "myctr"},
				Stderr: `not found: "myctr"`,
			},
			wantNotFound: true,
		},
		{
			name:            "malformed inspect",
			inspectStdout:   []byte(`{not json`),
			wantSyntaxError: true,
		},
		{
			name:            "null inspect",
			inspectStdout:   []byte(`null`),
			wantSchemaError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execInspectFailureRunner{
				execRunner: &execRunner{
					fakeRunner: newTestRunner(),
					execErr: &cli.CLIError{
						Args:     []string{"exec"},
						ExitCode: 1,
						Stderr:   "daemon unavailable",
					},
				},
				inspectStdout: tc.inspectStdout,
				inspectErr:    tc.inspectErr,
			}
			ctr := runTestContainer(t, f)
			_, _, err := ctr.Exec(context.Background(), []string{"true"})
			if err == nil {
				t.Fatal("Exec returned nil error for failed inspect verification")
			}
			if got := errors.Is(err, ErrContainerNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(ErrContainerNotFound) = %t, want %t: %v", got, tc.wantNotFound, err)
			}
			var syntaxErr *json.SyntaxError
			if got := errors.As(err, &syntaxErr); got != tc.wantSyntaxError {
				t.Errorf("errors.As(*json.SyntaxError) = %t, want %t: %v", got, tc.wantSyntaxError, err)
			}
			if tc.wantSchemaError && (!strings.Contains(err.Error(), "expected a JSON array") || errors.Is(err, ErrContainerNotFound)) {
				t.Errorf("error = %v, want a schema error distinct from ErrContainerNotFound", err)
			}
		})
	}
}

func TestExecAppNotFoundStderrIsResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "record not found"},
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec: %v, want app result", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
}

func TestExecSuccessAddsNoProbe(t *testing.T) {
	inner := &execRunner{fakeRunner: newTestRunner(), execStdout: "ok\n"}
	r := newCountingRunner(inner)
	ctr := runTestContainer(t, r)
	before := r.count()
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := r.count() - before; got != 1 {
		t.Fatalf("exec success calls = %d, want 1", got)
	}
}

func TestExecPassesOptionsAndEnvFile(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	_, _, err := ctr.Exec(context.Background(), []string{"id"},
		WithExecUser("nobody"), WithExecWorkDir("/tmp"),
		WithExecEnv(map[string]string{"TOKEN": "xyz"}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	joined := strings.Join(f.callWith("exec"), " ")
	if !strings.Contains(joined, "--user nobody") || !strings.Contains(joined, "--workdir /tmp") {
		t.Errorf("exec args = %s", joined)
	}
	if strings.Contains(joined, "--env ") {
		t.Errorf("exec env passed via argv: %s", joined)
	}
	if len(f.envFiles) != 1 || !strings.Contains(f.envFiles[0], "TOKEN=xyz\n") {
		t.Errorf("env-file captures = %q", f.envFiles)
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), nil); err == nil {
		t.Fatal("want error for empty command")
	}
}
