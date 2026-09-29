//go:build aix || dragonfly || freebsd || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func reaperProcessStartTime(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	psPath, err := trustedReaperHelperPath("ps")
	if err != nil {
		return "", err
	}
	lookupTimeout := 200 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", ctx.Err()
		}
		if remaining < lookupTimeout {
			lookupTimeout = remaining
		}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(lookupCtx, psPath, "-o", "lstart=", "-p", strconv.Itoa(pid))
	prepareReaperCommand(cmd)
	cmd.Cancel = func() error { return reaperCommandCancel(cmd) }
	cmd.WaitDelay = 50 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, 256))
	if len(data) == 256 && cmd.Process != nil {
		_ = reaperCommandCancel(cmd)
	}
	waitErr := cmd.Wait()
	if ctxErr := lookupCtx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if readErr != nil {
		return "", readErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return "", syscall.ESRCH
		}
		return "", waitErr
	}
	if len(data) == 256 {
		return "", fmt.Errorf("reaper: ps start time output exceeded limit")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("reaper: ps returned no start time for pid %d", pid)
	}
	return value, nil
}
