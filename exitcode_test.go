package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestExitCodeRequiresReportedStoppedStatus(t *testing.T) {
	for _, backend := range []string{"apple", "docker"} {
		for _, tc := range []struct {
			name, state, code string
			want              int
			unavailable       bool
		}{
			{"success", "stopped", "0", 0, false},
			{"failure", "stopped", "17", 17, false},
			{"running", "running", "0", 0, true},
			{"created", "created", "0", 0, true},
			{"missing", "stopped", "", 0, true},
			{"dead", "dead", "17", 17, false},
			{"dead-success", "dead", "0", 0, false},
			{"paused", "paused", "0", 0, true},
			{"restarting", "restarting", "0", 0, true},
			{"removing", "removing", "0", 0, true},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				if backend == "apple" && (tc.state == "dead" || tc.state == "paused" || tc.state == "restarting" || tc.state == "removing") {
					t.Skip("Docker-specific state")
				}
				f := newTestRunner()
				eng := engine(appleEngine{})
				if backend == "docker" {
					f = newDockerTestRunner()
					eng = dockerEngine{}
				}
				ctr := runTestContainer(t, f, withEngine(eng))
				code := ""
				if tc.code != "" {
					code = `,"exitCode":` + tc.code
				}
				f.inspectJSON = fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"labels":{"`+creationLabel+`":%q}},"status":{"state":%q%s}}]`, ctr.creation, tc.state, code)
				if backend == "docker" {
					state := tc.state
					if state == "stopped" {
						state = "exited"
					}
					f.inspectJSON = fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":%q%s},"Config":{"Image":"redis:7-alpine"}}]`, ctr.immutableID(), state, strings.ReplaceAll(code, "exitCode", "ExitCode"))
				}
				response := f.inspectJSON
				f.inspectJSON = strings.ReplaceAll(response, `"state":"stopped"`, `"state":"running"`)
				f.inspectJSON = strings.ReplaceAll(f.inspectJSON, `"Status":"exited"`, `"Status":"running"`)
				if _, err := ctr.State(context.Background()); err != nil {
					t.Fatalf("prime inspect: %v", err)
				}
				f.inspectJSON = response
				snapshot, err := ctr.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.ExitCodeReported() != (tc.code != "") {
					t.Fatalf("reported=%v", snapshot.ExitCodeReported())
				}
				got, err := ctr.ExitCode(context.Background())
				if tc.state != "stopped" && tc.state != "dead" && !errors.Is(err, ErrContainerNotStopped) {
					t.Fatalf("non-stopped error=%v", err)
				}
				if tc.state == "stopped" {
					waitCode, waitErr := ctr.Wait(context.Background())
					if tc.unavailable {
						if !errors.Is(waitErr, ErrExitCodeUnavailable) {
							t.Fatalf("Wait error=%v", waitErr)
						}
					} else if waitErr != nil || waitCode != tc.want {
						t.Fatalf("Wait=%d,%v", waitCode, waitErr)
					}
				}
				if tc.unavailable {
					if !errors.Is(err, ErrExitCodeUnavailable) {
						t.Fatalf("ExitCode error = %v, want ErrExitCodeUnavailable", err)
					}
				} else if err != nil || got != tc.want {
					t.Fatalf("ExitCode = %d, %v; want %d, nil", got, err, tc.want)
				}
			})
		}
	}
}

func TestExitCodePreservesInspectErrors(t *testing.T) {
	const secret = "opaque-diagnostic-value-cd41"
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithLabels(map[string]string{"purpose": secret}))
	f.failPrefix = "inspect"
	f.failStderr = "permission denied: " + secret
	for name, operation := range map[string]func() error{
		"ExitCode": func() error { _, err := ctr.ExitCode(context.Background()); return err },
		"State":    func() error { _, err := ctr.State(context.Background()); return err },
		"Inspect":  func() error { _, err := ctr.Inspect(context.Background()); return err },
		"Wait":     func() error { _, err := ctr.Wait(context.Background()); return err },
	} {
		err := operation()
		var failure *cli.CLIError
		if err == nil || strings.Contains(err.Error(), secret) || !errors.As(err, &failure) || failure.ExitCode != 1 {
			t.Fatalf("%s error=%v, want preserved redacted CLIError", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ctr.ExitCode(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExitCode error = %v, want context.Canceled", err)
	}
}

func TestWaitClassifiesTransientErrorsBeforeRedaction(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithLabels(map[string]string{"purpose": "busy"}))
	runner := &flakyInspectRunner{inner: f, errs: []error{&cli.CLIError{Binary: "container", Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "busy"}}, responses: []string{stoppedInspectResponse(ctr, 7)}}
	ctr.runner = runner
	code, err := ctr.Wait(context.Background())
	if err != nil || code != 7 {
		t.Fatalf("Wait=%d,%v, want retry then 7,nil", code, err)
	}
}

func TestPublicLifecyclePreservesRedactedPathError(t *testing.T) {
	const secret = "private-executable-path"
	raw := &os.PathError{Op: "fork/exec", Path: "/opt/" + secret + "/docker", Err: syscall.EACCES}
	ctr := &Container{id: "myctr", runner: waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) { return nil, nil, raw }), eng: dockerEngine{}, sync: &lockPack{uid: strings.Repeat("a", 64)}, diagnosticRedactorValue: cli.NewHashedContextRedactor(secret)}
	for name, op := range map[string]func() error{
		"State":    func() error { _, err := ctr.State(context.Background()); return err },
		"Wait":     func() error { _, err := ctr.Wait(context.Background()); return err },
		"Inspect":  func() error { _, err := ctr.Inspect(context.Background()); return err },
		"ExitCode": func() error { _, err := ctr.ExitCode(context.Background()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := op()
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || !errors.Is(err, os.ErrPermission) || !cli.PermanentStartError(err) {
				t.Fatalf("got %T %v, want typed permanent path error", err, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(pathErr.Path, secret) || strings.Contains(pathErr.Error(), secret) {
				t.Fatalf("path fields leak secret: %v", pathErr)
			}
			if !os.IsPermission(pathErr) || pathErr.Err != syscall.EACCES {
				t.Fatalf("path cause = %T %v, want original errno and os.IsPermission", pathErr.Err, pathErr.Err)
			}
			if pathErr == raw || !strings.Contains(raw.Path, secret) {
				t.Fatal("public error must be a redacted copy")
			}
		})
	}
}

func TestPublicLifecyclePreservesBareContextErrors(t *testing.T) {
	ctr := &Container{id: "myctr", eng: dockerEngine{}, sync: &lockPack{uid: strings.Repeat("a", 64)}, diagnosticRedactorValue: cli.NewHashedContextRedactor("context")}
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, ctx := range map[string]context.Context{"deadline": expired, "cancellation": canceled} {
		for operation, op := range map[string]func(context.Context) error{
			"State":    func(ctx context.Context) error { _, err := ctr.State(ctx); return err },
			"Wait":     func(ctx context.Context) error { _, err := ctr.Wait(ctx); return err },
			"Inspect":  func(ctx context.Context) error { _, err := ctr.Inspect(ctx); return err },
			"ExitCode": func(ctx context.Context) error { _, err := ctr.ExitCode(ctx); return err },
		} {
			t.Run(name+"/"+operation, func(t *testing.T) {
				err := op(ctx)
				if err != ctx.Err() {
					t.Fatalf("error = %T %v, want original %v", err, err, ctx.Err())
				}
				if name == "deadline" {
					var timeout interface{ Timeout() bool }
					if !os.IsTimeout(err) || !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatalf("error lost structured timeout: %v", err)
					}
				}
			})
		}
	}
}

func TestStatePreservesInFlightDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	const secret = "private-inspect-timeout-value"
	binary := filepath.Join(t.TempDir(), "docker")
	started := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\nprintf '" + secret + "' >&2\nprintf started > \"$INSPECT_STARTED\"\nexec sleep 30\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runner := &cli.ExecRunner{Binary: binary, Identity: "docker", Env: []string{"INSPECT_STARTED=" + started}}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}, sync: &lockPack{uid: strings.Repeat("a", 64)}, diagnosticRedactorValue: cli.NewHashedContextRedactor(secret)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := ctr.State(ctx)
	if _, startErr := os.Stat(started); startErr != nil {
		t.Fatalf("inspect never started: %v", startErr)
	}
	var timeout interface{ Timeout() bool }
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("State lost in-flight timeout semantics: %T %v", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("timeout diagnostic leaked secret")
	}
}
