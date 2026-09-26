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
//
// It uses cmd.Wait rather than cmd.Process.Wait because Wait is what closes
// the parent side of a StdoutPipe and joins exec's I/O copy goroutines;
// bypassing it leaks both for the life of the test binary.
func (g *ChildGroup) Kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, cmd := range g.started {
		if cmd.Process == nil {
			continue
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
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
//
// Whatever the child managed to write is returned even on timeout. A hang is
// exactly when the partial output is the only diagnostic available, so
// dropping it would leave nothing to explain the failure.
func ReadReady(r io.Reader, timeout time.Duration) (string, error) {
	// The single reader publishes each chunk, so the timeout path can report
	// what the child produced before it went quiet.
	type chunk struct {
		text string
		done bool
		err  error
	}
	ch := make(chan chunk, 1)
	go func() {
		var acc string
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") || strings.Contains(acc, "CHILD-ERROR:") || err != nil {
				// Non-blocking: the caller may already have timed out, and a
				// blocked send would wedge this goroutine forever.
				select {
				case ch <- chunk{text: acc, done: true, err: err}:
				default:
				}
				return
			}
			select {
			case ch <- chunk{text: acc}:
			default:
				// The caller is behind. Overwrite the stale snapshot rather
				// than dropping the newer text, so the reported output is
				// never older than what the child has already written.
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- chunk{text: acc}:
				default:
				}
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var seen string
	for {
		select {
		case c := <-ch:
			seen = c.text
			if c.done {
				// A read that ended without a marker is a failure, and the
				// read error is the only explanation available.
				if c.err != nil && !strings.Contains(seen, "READY:") && !strings.Contains(seen, "CHILD-ERROR:") {
					return seen, fmt.Errorf("child read ended before reporting readiness: %w", c.err)
				}
				return seen, nil
			}
		case <-timer.C:
			// Release the reader: the blocked Read has no cancellation of
			// its own, and a grandchild holding the pipe write end would
			// otherwise keep this goroutine and the pipe alive for the rest
			// of the test binary's run.
			if closer, ok := r.(io.Closer); ok {
				_ = closer.Close()
			}
			return seen, fmt.Errorf("child did not report readiness within %v", timeout)
		}
	}
}
