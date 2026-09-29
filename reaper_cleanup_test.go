//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const reaperCleanupTimeout = 2 * time.Second

// killForTest kills the reaper process group and waits until the child is
// reaped, so fake descendants cannot survive the test and the next write
// deterministically fails. The cleanup is bounded because a test must not
// be able to wedge the whole suite on a stalled lookup or an expanding tree.
func (r *reaper) killForTest() error {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	if cmd != nil {
		if err := killReaperCommand(ctx, cmd); err != nil {
			log.Printf("container-go: reaper cleanup: %v", err)
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if exited != nil {
		select {
		case <-exited:
		case <-ctx.Done():
			cleanupErrors = append(cleanupErrors, fmt.Errorf("reaper: cleanup timed out: %w", ctx.Err()))
		}
	}
	return errors.Join(cleanupErrors...)
}
