//go:build !windows

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// reaperDeleteGateScenario is one delete-gate fixture. Each scenario runs
// through the real parent registration, the self-executed reaper child, and a
// fake backend CLI, so the parent-side event protocol and the child-side gate
// are both exercised.
type reaperDeleteGateScenario struct {
	name            string
	subcommand      string
	inspectJSON     string
	inspectFailures int // -1 always fails, 0 never fails, N fails N times
	pending         bool
	wantDelete      string // empty means no delete expected
}

func TestNativeReaperSelfExecDeleteGate(t *testing.T) {
	requirePOSIXShell(t)
	creation := strings.Repeat("a", 16)
	uid := strings.Repeat("c", 64)
	name := "delete-gate-name"
	matchingDocker := fmt.Sprintf(`[{"Id":%q,"Config":{"Labels":{%q:%q}}}]`, uid, creationLabel, creation)
	mismatchedDocker := fmt.Sprintf(`[{"Id":%q,"Config":{"Labels":{%q:%q}}}]`, uid, creationLabel, strings.Repeat("d", 16))
	matchingApple := fmt.Sprintf(`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation)

	scenarios := []reaperDeleteGateScenario{
		{name: "docker-name-match", subcommand: "rm", inspectJSON: matchingDocker, wantDelete: "--force " + uid},
		{name: "docker-name-absent", subcommand: "rm", inspectJSON: "[]"},
		{name: "docker-name-mismatch", subcommand: "rm", inspectJSON: mismatchedDocker},
		{name: "docker-name-inspect-failure", subcommand: "rm", inspectFailures: -1},
		{name: "docker-name-pending", subcommand: "rm", inspectJSON: matchingDocker, inspectFailures: 1, pending: true, wantDelete: "--force " + uid},
		{name: "apple-name-match", subcommand: "delete", inspectJSON: matchingApple, wantDelete: "--force " + name},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			binary, deleteLog, inspectLog := writeReaperDeleteGateBackend(t, dir, scenario)
			r := newReaper(binary, scenario.subcommand)
			r.pendingAttempts = 2
			var err error
			if scenario.pending {
				err = r.registerPending(name, creation)
			} else {
				err = r.register(name, creation)
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			r.closeStdin()
			waitForReaperTestExit(t, r)

			got := readNativeReaperLog(t, deleteLog)
			if scenario.wantDelete == "" {
				if len(got) != 0 {
					t.Fatalf("deleted %v, want no delete", got)
				}
				return
			}
			want := []string{scenario.wantDelete}
			if !slices.Equal(got, want) {
				t.Fatalf("deleted %v, want %v", got, want)
			}
			if calls := len(readNativeReaperLog(t, inspectLog)); calls == 0 {
				t.Fatal("deleted without inspecting the generation")
			}
		})
	}
}

// writeReaperDeleteGateBackend installs a fake CLI and returns its delete log
// and inspect log. The script answers inspect with the scenario's JSON after
// the configured number of failures, and records delete argv.
func writeReaperDeleteGateBackend(t *testing.T, dir string, scenario reaperDeleteGateScenario) (binary, deleteLog, inspectLog string) {
	t.Helper()
	deleteLog = filepath.Join(dir, "delete.log")
	inspectLog = filepath.Join(dir, "inspect.log")
	counter := filepath.Join(dir, "inspect-count")

	var failure string
	switch {
	case scenario.inspectFailures < 0:
		failure = "exit 1"
	case scenario.inspectFailures > 0:
		failure = fmt.Sprintf(`count=0
[ -f %[1]s ] && count=$(cat %[1]s)
count=$((count + 1))
printf '%%s\n' "$count" > %[1]s
[ "$count" -le %[2]d ] && exit 1`, counter, scenario.inspectFailures)
	}

	body := "#!/bin/sh\n" +
		"sub=\"$1\"\n" +
		"shift\n" +
		"if [ \"$sub\" = \"inspect\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> \"" + inspectLog + "\"\n" +
		indentShell(failure) +
		"  cat <<'JSON'\n" +
		scenario.inspectJSON + "\n" +
		"JSON\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s\\n' \"$*\" >> \"" + deleteLog + "\"\n" +
		"exit 0\n"
	binary = filepath.Join(dir, "backend")
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, deleteLog, inspectLog
}

func indentShell(script string) string {
	if script == "" {
		return ""
	}
	var out strings.Builder
	for _, line := range strings.Split(script, "\n") {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
