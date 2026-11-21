package container

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// The native reaper is started by re-executing this executable with an
// internal marker and a fixed argv. The marker and argv are written only by
// this library, so an ordinary process never enters the loop even though the
// hook lives in an init function.
func init() {
	if !isInternalReaperInvocation(os.Getenv(reaperInternalEnv), os.Args) {
		return
	}
	os.Exit(runNativeReaperMain(os.Args[2:], os.Stderr))
}

// isInternalReaperInvocation requires both the internal environment marker
// and the internal argv command, so a stray variable or a coincidental first
// argument alone cannot turn a host process into a reaper.
func isInternalReaperInvocation(marker string, args []string) bool {
	return marker == "1" && len(args) >= 2 && args[1] == reaperInternalCommand
}

// runNativeReaperMain parses the internal argv:
// <backend binary> <delete subcommand> <creation label> <timeout> <pending attempts> [delete flags...]
func runNativeReaperMain(args []string, stderr io.Writer) int {
	if len(args) < 5 {
		_, _ = fmt.Fprintln(stderr, "container-go: native reaper: invalid arguments")
		return 2
	}
	timeoutSeconds, err := strconv.Atoi(args[3])
	if err != nil {
		timeoutSeconds = 0
	}
	pendingAttempts, err := strconv.Atoi(args[4])
	if err != nil {
		pendingAttempts = 0
	}
	cfg := nativeReaperConfig{
		binary:          args[0],
		subcommand:      args[1],
		key:             args[2],
		timeout:         time.Duration(timeoutSeconds) * time.Second,
		pendingAttempts: pendingAttempts,
		deleteFlags:     append([]string(nil), args[5:]...),
	}
	return runNativeReaper(cfg, os.Stdin, os.Stderr)
}
