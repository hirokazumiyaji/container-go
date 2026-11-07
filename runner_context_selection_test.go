package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestRunnerEmptyContextSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "amb")
	for _, tc := range []struct{ name, entry, pinned string }{
		{"empty-assignment", "DOCKER_CONTEXT=", "active"},
		{"bare-removal", "DOCKER_CONTEXT", "amb"},
	} {
		for _, operation := range []string{"pull", "package-prune", "client-prune"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				dir := t.TempDir()
				binary, log := filepath.Join(dir, "docker"), filepath.Join(dir, "env.log")
				script := `#!/bin/sh
case "$1" in
context)
 printf 'lookup=%s\ninspect=%s\n' "$*" "${DOCKER_CONTEXT-unset}" >> "$TARGET_ENV_LOG"
 printf '[{"Name":"active","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]'
 ;;
pull|ps)
 printf 'operation=%s\n' "${DOCKER_CONTEXT-unset}" >> "$TARGET_ENV_LOG"
 ;;
*) exit 1 ;;
esac
`
				if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				runner := &cli.ExecRunner{Binary: binary, Env: []string{"TARGET_ENV_LOG=" + log, tc.entry}}
				var err error
				switch operation {
				case "pull":
					err = Pull(context.Background(), "example:test", WithPullRunner(runner))
				case "package-prune":
					_, err = Prune(context.Background(), WithPruneRunner(runner))
				case "client-prune":
					var client *Client
					client, err = NewClient(WithClientBackend("docker"), WithClientRunner(runner))
					if err == nil {
						_, err = client.Prune(context.Background())
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				inspect := ""
				if tc.entry == "DOCKER_CONTEXT" {
					inspect = "unset"
				}
				target := tc.pinned
				if operation == "package-prune" {
					target = inspect
				}
				if operation == "client-prune" {
					inspect, target = "amb", "amb"
				}
				lookup := "context inspect"
				if operation == "client-prune" || (operation == "pull" && tc.entry == "DOCKER_CONTEXT") {
					lookup += " amb"
				}
				want := fmt.Sprintf("lookup=%s\ninspect=%s\noperation=%s\n", lookup, inspect, target)
				if string(got) != want {
					t.Fatalf("child environments=%q, want %q", got, want)
				}
			})
		}
	}
}

func TestPullBareContextMarkerKeepsAmbientTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_CONTEXT", "amb")
	for _, host := range []string{"", "tcp://ambient-host:2375"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			dir := t.TempDir()
			binary, log := filepath.Join(dir, "docker"), filepath.Join(dir, "calls.log")
			script := `#!/bin/sh
case "$1" in
context)
 printf '%s\n' "$*" >> "$TARGET_ENV_LOG"
 printf '[{"Name":"amb","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]'
 ;;
pull) printf 'pull context=%s\n' "$DOCKER_CONTEXT" >> "$TARGET_ENV_LOG" ;;
*) exit 1 ;;
esac
`
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &cli.ExecRunner{Binary: binary, Env: []string{"TARGET_ENV_LOG=" + log, "DOCKER_CONTEXT"}}
			if err := Pull(context.Background(), "example:test", WithPullRunner(runner)); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if want := "context inspect amb\npull context=amb\n"; string(got) != want {
				t.Fatalf("commands=%q, want %q", got, want)
			}
		})
	}
}
