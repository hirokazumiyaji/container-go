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
	probeErr    error
	probeStdout string
	calls       []string
}

func (r *classifyProbeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	err := r.probeErr
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) && cliErr.Binary == "" {
		if len(args) > 0 && args[0] == "version" {
			cliErr.Binary = "docker"
		} else {
			cliErr.Binary = "container"
		}
	}
	return []byte(r.probeStdout), nil, err
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
	}
	original := &cli.CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	runner := &classifyProbeRunner{probeErr: probeErr, probeStdout: string(stdout)}

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

func TestClassifyDaemonDownWithTermsInEndpointPaths(t *testing.T) {
	cases := []struct {
		name       string
		diagnostic string
	}{
		{
			name:       "config",
			diagnostic: "Cannot connect to the Docker daemon at unix:///Users/test/.config/containers/run/docker.sock: connect: connection refused",
		},
		{
			name:       "permission",
			diagnostic: "Cannot connect to the Docker daemon at unix:///Volumes/permission/docker.sock: connect: connection refused",
		},
		{
			name:       "certificate",
			diagnostic: "Cannot connect to the Docker daemon at tcp://certificate.internal:2376: connect: connection refused",
		},
		{
			name:       "tls",
			diagnostic: "Cannot connect to the Docker daemon at tcp://tls.internal:2376: connect: connection refused",
		},
		{
			name:       "x509",
			diagnostic: "Cannot connect to the Docker daemon at tcp://x509.internal:2376: connect: connection refused",
		},
		{
			name:       "tls port",
			diagnostic: "Cannot connect to the Docker daemon at tcp://tls:2376: connect: connection refused",
		},
		{
			name:       "x509 port",
			diagnostic: "Cannot connect to the Docker daemon at tcp://x509:2376: connect: connection refused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := &cli.CLIError{
				Binary:   "docker",
				Args:     []string{"version"},
				ExitCode: 1,
				Stderr:   tc.diagnostic,
			}
			probeErr := &cli.CLIError{
				Binary:   "docker",
				Args:     []string{"version", "--format", "{{.Server.Version}}"},
				ExitCode: 1,
				Stderr:   tc.diagnostic,
			}
			runner := &classifyProbeRunner{probeErr: probeErr}

			got := cli.Classify(context.Background(), runner, original, dockerEngine{}.probe())
			if !errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("error = %v, want daemon-down classification for endpoint path", got)
			}
		})
	}
}

func TestClassifyDockerDesktopUnableToStart(t *testing.T) {
	original := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"run"},
		ExitCode: 1,
		Stderr:   "command failed",
	}
	probeErr := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"version", "--format", "{{.Server.Version}}"},
		ExitCode: 1,
		Stderr:   "Error response from daemon: Docker Desktop is unable to start",
	}
	runner := &classifyProbeRunner{probeErr: probeErr}

	got := cli.Classify(context.Background(), runner, original, dockerEngine{}.probe())
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning for Docker Desktop startup failure", got)
	}
	if !errors.Is(got, original) || !errors.Is(got, probeErr) {
		t.Fatalf("error = %v, want original and probe error chains", got)
	}
}

func TestDockerProbeUnavailableRequiresStructuredDesktopPhrase(t *testing.T) {
	for _, stderr := range []string{
		"Error response from daemon: unable to start",
		"Error response from daemon: Docker Desktop is unable to startup",
		"daemon: Docker Desktop is unable to start",
		"application says Docker Desktop is unable to start",
	} {
		t.Run(stderr, func(t *testing.T) {
			err := &cli.CLIError{
				Binary:   "docker",
				Args:     []string{"version"},
				ExitCode: 1,
				Stderr:   stderr,
			}
			if dockerProbeUnavailable(err) {
				t.Fatalf("dockerProbeUnavailable(%q) = true, want false", stderr)
			}
		})
	}
}

func TestClassifyExplicitProbeDoesNotTreatBareConfigAsNonLiveness(t *testing.T) {
	orig := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"},
		ExitCode: 1, Stderr: "configuration changed while the command was running",
	}
	probeErr := &cli.CLIError{
		Binary: "container", Args: []string{"system", "status"},
		ExitCode: 1, Stderr: "XPC connection error",
	}
	runner := &classifyProbeRunner{probeErr: probeErr}
	probe := appleEngine{}.probe()
	probe.IsUnavailable = func(error) bool { return true }

	got := cli.Classify(context.Background(), runner, orig, probe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want explicit probe liveness evidence", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("error = %v, want original and probe chains", got)
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
					wantSystemDown: true,
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
