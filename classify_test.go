package container

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type classifyProbeRunner struct {
	probeErr error
	calls    []string
}

func (r *classifyProbeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil, nil, r.probeErr
}

func TestAppleSystemStatusRealStdoutClassifiesProbeFailure(t *testing.T) {
	// Apple Container 1.3.0 reports this diagnostic on stdout while
	// `system status` exits unsuccessfully.
	stdout, err := os.ReadFile("testdata/apple_system_status_1.3.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	probeErr := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"system", "status"},
		ExitCode: 1,
		Stdout:   string(stdout),
	}
	original := &cli.CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	runner := &classifyProbeRunner{probeErr: probeErr}

	got := cli.Classify(context.Background(), runner, original, appleEngine{}.probe())
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning from real Apple stdout", got)
	}
	if !errors.Is(got, original) || !errors.Is(got, probeErr) {
		t.Fatalf("error = %v, want original and probe error chains", got)
	}
	if !strings.Contains(got.Error(), "apiserver is not running and not registered with launchd") {
		t.Errorf("error = %q, want real Apple diagnostic", got)
	}
}

func TestClassifyProbeFailureMatrix(t *testing.T) {
	backends := []struct {
		name       string
		probe      cli.Probe
		downStderr string
	}{
		{
			name:       "apple",
			probe:      appleEngine{}.probe(),
			downStderr: "XPC connection error",
		},
		{
			name:       "docker",
			probe:      dockerEngine{}.probe(),
			downStderr: "Cannot connect to the Docker daemon",
		},
	}

	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			cases := []struct {
				name           string
				originalStderr string
				probeErr       error
				wantSystemDown bool
			}{
				{
					name:           "daemon down",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
					wantSystemDown: true,
				},
				{
					name:           "permission denied",
					originalStderr: "permission denied",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "invalid configuration",
					originalStderr: "invalid configuration",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "TLS configuration",
					originalStderr: "tls handshake timeout",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "x509 certificate",
					originalStderr: "x509: certificate signed by unknown authority",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "credential helper",
					originalStderr: "error getting credentials: docker-credential helper failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "invalid flag",
					originalStderr: "unknown flag: --not-a-real-flag",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   backend.downStderr,
					},
				},
				{
					name:           "probe permission error",
					originalStderr: "command failed",
					probeErr:       errors.New("permission denied"),
				},
				{
					name:           "probe permission CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "permission denied",
					},
				},
				{
					name:           "probe configuration CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "invalid configuration",
					},
				},
				{
					name:           "probe TLS CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "tls handshake timeout: error during connect",
					},
				},
				{
					name:           "probe x509 CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "x509: certificate signed by unknown authority",
					},
				},
				{
					name:           "probe credential helper CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "error getting credentials - err: exit status 1",
					},
				},
				{
					name:           "probe invalid flag CLI error",
					originalStderr: "command failed",
					probeErr: &cli.CLIError{
						Args:     backend.probe.Args,
						ExitCode: 1,
						Stderr:   "unknown flag: --not-a-real-flag",
					},
				},
				{
					name:           "probe cancellation",
					originalStderr: "command failed",
					probeErr:       context.Canceled,
				},
				{
					name:           "probe deadline error",
					originalStderr: "command failed",
					probeErr:       context.DeadlineExceeded,
				},
			}

			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					original := &cli.CLIError{
						Args:     []string{"run"},
						ExitCode: 1,
						Stderr:   tc.originalStderr,
					}
					runner := &classifyProbeRunner{probeErr: tc.probeErr}

					got := cli.Classify(context.Background(), runner, original, backend.probe)
					if !errors.Is(got, original) {
						t.Errorf("error = %v, want original error", got)
					}
					if !errors.Is(got, tc.probeErr) {
						t.Errorf("error = %v, want probe error", got)
					}
					var gotCLI *cli.CLIError
					if !errors.As(got, &gotCLI) || gotCLI != original {
						t.Errorf("errors.As(*CLIError) = %v, want original CLIError", gotCLI)
					}
					if gotSystem := errors.Is(got, ErrSystemNotRunning); gotSystem != tc.wantSystemDown {
						t.Errorf("errors.Is(ErrSystemNotRunning) = %t, want %t (error: %v)", gotSystem, tc.wantSystemDown, got)
					}
					if tc.wantSystemDown && !strings.Contains(got.Error(), backend.probe.Hint) {
						t.Errorf("error = %q, want probe hint %q", got, backend.probe.Hint)
					}
					wantCall := strings.Join(backend.probe.Args, " ")
					if len(runner.calls) != 1 || runner.calls[0] != wantCall {
						t.Errorf("probe calls = %v, want [%q]", runner.calls, wantCall)
					}
				})
			}
		})
	}
}
