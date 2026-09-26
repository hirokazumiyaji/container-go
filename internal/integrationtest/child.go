package integrationtest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChildReadyTimeout bounds how long a cross-process test waits for a child
// to announce readiness. The child may legitimately be silent, so the wait
// must be enforced while blocked on the read, not before it.
//
// A var, not a const, so tests can shrink it.
var ChildReadyTimeout = 3 * time.Minute

// Nonce returns a short random token unique to this run. Integration
// resource names and reuse groups are scoped with it so two concurrent runs
// of the same suite cannot prune or delete each other's containers.
func Nonce() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A weaker nonce only risks colliding with another concurrent run,
		// never within one, so a timestamp is an acceptable fallback.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// ChildGroup is a set of helper child processes. Kill is safe to call more
// than once and reaps every started child, so a partial start failure does
// not leak a running child.
type ChildGroup struct {
	mu      sync.Mutex
	started []*exec.Cmd
}

// Track registers a child for cleanup. Call it as each child starts, so a
// later start failure still reaps the earlier ones.
func (g *ChildGroup) Track(cmd *exec.Cmd) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.started = append(g.started, cmd)
}

// Kill terminates and reaps every tracked child.
func (g *ChildGroup) Kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, cmd := range g.started {
		if cmd.Process == nil {
			continue
		}
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// ReadReady reads from r until one of the ready markers appears, the reader
// fails, or the timeout elapses.
//
// The read runs on its own goroutine so the deadline is enforced while
// blocked: a child that never writes would otherwise hold the test until the
// package-wide timeout, and the declared bound would be a lie. The timeout
// is returned as an error rather than reported through t, so the caller
// decides how to fail.
func ReadReady(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		out string
	}
	ch := make(chan result, 1)
	go func() {
		var acc string
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") || strings.Contains(acc, "CHILD-ERROR:") || err != nil {
				ch <- result{out: acc}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.out, nil
	case <-timer.C:
		return "", fmt.Errorf("child did not report readiness within %v", timeout)
	}
}
