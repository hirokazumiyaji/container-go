package container

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
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
// Each stdin line is "id|creation": before deleting, the script
// inspects the current container and skips the delete when the
// creation generation no longer matches, so a same-name replacement
// created after registration survives the old reaper.
const reaperScript = `set -f
bin="$1"
sub="$2"
entries=""
while IFS= read -r line; do
  entries="$entries $line"
done
for entry in $entries; do
  id=${entry%%|*}
  creation=${entry##*|}
  if [ -n "$creation" ] && [ "$creation" != "$id" ]; then
    if ! "$bin" inspect "$id" 2>/dev/null | grep -q "$creation"; then
      continue
    fi
  fi
  "$bin" "$sub" --force "$id" >/dev/null 2>&1 || true
done
`

const maxReaperSpawnFailures = 3

// creationRE validates the hex generation ID passed to the reaper.
var creationRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

type reaperEntry struct {
	id       string
	creation string
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{binary: binary, subcommand: subcommand}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel; empty skips the generation check for
// backward compatibility.
func (r *reaper) register(id, creation string) error {
	if !nameRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation}
	r.entries = append(r.entries, entry)
	line := reaperLine(entry)
	if r.stdin != nil {
		if r.writeLocked(line) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func reaperLine(e reaperEntry) string {
	if e.creation == "" {
		return e.id + "|"
	}
	return e.id + "|" + e.creation
}

func (r *reaper) writeLocked(line string) error {
	_, err := io.WriteString(r.stdin, line+"\n")
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
		for _, e := range r.entries {
			if r.writeLocked(reaperLine(e)) != nil {
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
func registerWithGlobalReaper(binary, subcommand, id, creation string) {
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
	_ = r.register(id, creation)
}
