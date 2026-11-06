package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
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
				got, err := ctr.ExitCode(context.Background())
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
	const secret = "exitcode-inspect-secret"
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithLabels(map[string]string{"purpose": secret}))
	f.failPrefix = "inspect"
	f.failStderr = "permission denied: " + secret
	if _, err := ctr.ExitCode(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("ExitCode inspect error = %v, want redacted failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ctr.ExitCode(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExitCode error = %v, want context.Canceled", err)
	}
}
