//go:build !windows

package container

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type dockerPruneReaperRunner struct {
	bin string
	uid string

	mu      sync.Mutex
	deleted []string
}

func (r *dockerPruneReaperRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(r.uid + "\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(
			`[{"Id":%q,"Name":"/prune-reaper","State":{"Status":"exited"},"Config":{"Labels":{%q:"true",%q:"0123456789abcdef"}}}]`,
			r.uid, managedLabel, creationLabel,
		)), nil, nil
	case "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *dockerPruneReaperRunner) External() bool         { return true }
func (r *dockerPruneReaperRunner) ExternalBinary() string { return r.bin }

func TestDockerPruneUnregistersNameBasedReaper(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin := filepath.Join(t.TempDir(), "docker")
	uid := strings.Repeat("a", 64)
	runner := &dockerPruneReaperRunner{bin: bin, uid: uid}
	eng := dockerEngine{}
	target, ok := runReaperTarget(&config{runner: runner, eng: eng})
	if !ok {
		t.Fatal("runReaperTarget returned false for an external Docker runner")
	}

	watchdog := newReaper(bin, "rm", dockerDeleteVolumesFlag)
	if err := watchdog.register("prune-reaper", "0123456789abcdef"); err != nil {
		t.Fatalf("register name entry: %v", err)
	}
	if err := watchdog.register(uid, ""); err != nil {
		t.Fatalf("register immutable entry: %v", err)
	}
	globalReapersMu.Lock()
	globalReapers[target.reaperKey] = watchdog
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, target.reaperKey)
		globalReapersMu.Unlock()
		watchdog.closeStdin()
	})

	removed, err := pruneWith(context.Background(), runner, eng)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != uid {
		t.Fatalf("removed = %v, want [%s]", removed, uid)
	}
	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != uid {
		t.Fatalf("deleted = %v, want [%s]", deleted, uid)
	}

	watchdog.mu.Lock()
	entries := append([]reaperEntry(nil), watchdog.entries...)
	watchdog.mu.Unlock()
	if len(entries) != 0 {
		t.Fatalf("reaper retained %d records after prune: %+v", len(entries), entries)
	}
}
