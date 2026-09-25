package container

import (
	"context"
	"errors"
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
