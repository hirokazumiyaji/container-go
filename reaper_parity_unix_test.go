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

// reaperParityScenario is one delete-gate fixture. The same shell script and
// the same registered entries are driven through both the /bin/sh reaper and
// the native reaper, so a behavior change in either implementation fails the
// parity check.
type reaperParityScenario struct {
	name            string
	subcommand      string
	inspectJSON     string
	inspectFailures int // -1 always fails, 0 never fails, N fails N times
	pending         bool
	wantDelete      string // empty means no delete expected
}

func TestReaperImplementationsShareTheDeleteGate(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("subprocess fixture matrix; the quick native tests cover the worker pool under -race")
	}
	requirePOSIXShell(t)
	creation := strings.Repeat("a", 16)
	uid := strings.Repeat("c", 64)
	name := "parity-name"
	matchingDocker := fmt.Sprintf(`[{"Id":%q,"Config":{"Labels":{%q:%q}}}]`, uid, creationLabel, creation)
	mismatchedDocker := fmt.Sprintf(`[{"Id":%q,"Config":{"Labels":{%q:%q}}}]`, uid, creationLabel, strings.Repeat("d", 16))
	matchingApple := fmt.Sprintf(`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation)

	scenarios := []reaperParityScenario{
		{name: "docker-name-match", subcommand: "rm", inspectJSON: matchingDocker, wantDelete: "--force " + uid},
		{name: "docker-name-absent", subcommand: "rm", inspectJSON: "[]"},
		{name: "docker-name-mismatch", subcommand: "rm", inspectJSON: mismatchedDocker},
		{name: "docker-name-inspect-failure", subcommand: "rm", inspectFailures: -1},
		{name: "docker-name-pending", subcommand: "rm", inspectJSON: matchingDocker, inspectFailures: 1, pending: true, wantDelete: "--force " + uid},
		{name: "apple-name-match", subcommand: "delete", inspectJSON: matchingApple, wantDelete: "--force " + name},
	}
	implementations := []struct {
		name  string
		build func(binary, subcommand string) *reaper
	}{
		{name: "shell", build: func(binary, subcommand string) *reaper { return newReaper(binary, subcommand) }},
		{name: "native", build: func(binary, subcommand string) *reaper { return newNativeReaper(binary, subcommand) }},
	}

	for _, implementation := range implementations {
		for _, scenario := range scenarios {
			t.Run(implementation.name+"/"+scenario.name, func(t *testing.T) {
				dir := t.TempDir()
				binary, deleteLog, inspectLog := writeReaperParityBackend(t, dir, scenario)
				r := implementation.build(binary, scenario.subcommand)
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
}

// writeReaperParityBackend installs a fake CLI and returns its delete log and
// inspect log. The script answers inspect with the scenario's JSON after the
// configured number of failures, and records delete argv.
func writeReaperParityBackend(t *testing.T, dir string, scenario reaperParityScenario) (binary, deleteLog, inspectLog string) {
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
