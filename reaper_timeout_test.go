package container

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// run_with_timeout backgrounds a shell function, so $! is the subshell rather
// than the backend CLI. Without job control, killing that subshell left the
// CLI alive holding the command-substitution pipe, so the assignment never
// returned and the per-entry timeout bounded nothing. An unresponsive daemon
// therefore wedged the reaper and every later entry went unreaped.
func TestReviewReaperTimeoutBoundsHungCLI(t *testing.T) {
	script := reaperScript
	if !strings.Contains(script, "set -m") {
		t.Fatal("reaper script does not enable job control for the backgrounded function")
	}
	if !strings.Contains(script, `kill -9 -"$command_pid"`) {
		t.Fatal("reaper script does not signal the background job's process group")
	}
}

// The helper must return promptly when the command hangs, rather than waiting
// for the hung grandchild to release the pipe.
func TestReviewRunWithTimeoutReturnsForHungCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	// A function that never returns, standing in for a wedged backend CLI.
	harness := `hang() { sleep 300; }
run_with_timeout() {
  seconds="$1"
  shift
  set -m
  "$@" & command_pid=$!
  (sleep "$seconds"; kill -9 -"$command_pid" 2>/dev/null || kill -9 "$command_pid" 2>/dev/null || true) >/dev/null 2>&1 & killer_pid=$!
  wait "$command_pid" 2>/dev/null
  rc=$?
  set +m
  kill -9 "$killer_pid" 2>/dev/null || true
  wait "$killer_pid" 2>/dev/null || true
  return "$rc"
}
run_with_timeout 1 hang
echo "returned rc=$?"`

	done := make(chan struct{})
	var out string
	go func() {
		cmd := exec.Command("/bin/sh", "-c", harness)
		b, err := cmd.CombinedOutput()
		out = string(b)
		_ = err
		close(done)
	}()

	select {
	case <-done:
		if !strings.Contains(out, "returned") {
			t.Fatalf("helper did not return cleanly: %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run_with_timeout did not bound a hung command: %q", out)
	}
}
