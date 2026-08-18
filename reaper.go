package container

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sync"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated against Apple Container's name rule, and the script
// itself disables globbing and quotes every expansion the IDs reach.
const reaperScript = `set -f
bin="$1"
sub="$2"
ids=""
while IFS= read -r id; do
  ids="$ids $id"
done
for id in $ids; do
  "$bin" "$sub" --force "$id" >/dev/null 2>&1 || true
done
`

const maxReaperSpawnFailures = 3

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	ids           []string
	spawnFailures int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{binary: binary, subcommand: subcommand}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed.
func (r *reaper) register(id string) error {
	if !nameRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	if r.stdin != nil {
		if r.writeLocked(id) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) writeLocked(id string) error {
	_, err := io.WriteString(r.stdin, id+"\n")
	return err
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every known ID with it.
func (r *reaper) respawnAndReplayLocked() error {
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			continue
		}
		replayed := true
		for _, id := range r.ids {
			if r.writeLocked(id) != nil {
				replayed = false
				break
			}
		}
		if replayed {
			return nil
		}
		r.spawnFailures++
	}
	return errors.New("reaper: giving up after repeated spawn failures")
}

func (r *reaper) spawnLocked() error {
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	return nil
}

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
}

// killForTest kills the reaper child and waits until it is reaped, so
// the next write deterministically fails.
func (r *reaper) killForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		_ = r.cmd.Process.Kill()
		<-r.exited
	}
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble never
// fails container startup. The reaper needs /bin/sh, so on Windows
// this is a no-op and cleanup relies on the normal paths.
func registerWithGlobalReaper(binary, subcommand, id string) {
	if runtime.GOOS == "windows" {
		return
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	globalReapersMu.Unlock()
	_ = r.register(id)
}
