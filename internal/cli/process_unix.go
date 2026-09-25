//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// parentDeathScript is intentionally fixed. The backend binary and its
// arguments arrive as positional parameters, so no caller-controlled shell
// text is assembled. The read end of the parent pipe is fd 3 in the child.
const parentDeathScript = `
set +m
bin=$1
shift
"$bin" "$@" 3<&- &
child=$!
kill_descendants() {
  children=$(pgrep -P "$1" 2>/dev/null) || children=
  for descendant in $children; do
    kill_descendants "$descendant"
    kill -9 "$descendant" 2>/dev/null || true
  done
}
(
  IFS= read -r _ <&3
  kill_descendants "$child"
  kill -KILL "-$$" 2>/dev/null || kill -KILL "$child" 2>/dev/null || true
) &
watcher=$!
wait "$child"
rc=$?
kill "$watcher" 2>/dev/null || true
wait "$watcher" 2>/dev/null || true
exit "$rc"
`

// commandWithParentDeath creates a supervisor whose process group contains
// the backend invocation. The parent retains the write end until the call
// returns; closing the process therefore makes the supervisor kill the whole
// group. The target does not inherit the read descriptor, which keeps a
// daemonized descendant from delaying the death watch indefinitely.
func commandWithParentDeath(ctx context.Context, binary string, args []string) (*exec.Cmd, func(), error) {
	parentRead, parentWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "-c", parentDeathScript, "containergo-parent-watch", binary)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", argv...)
	cmd.ExtraFiles = []*os.File{parentRead}
	cleanup := func() {
		_ = parentRead.Close()
		_ = parentWrite.Close()
	}
	return cmd, cleanup, nil
}

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// The group may have disappeared between the signal and this fallback.
	// The direct child is still owned by os/exec at this point.
	return cmd.Process.Kill()
}
