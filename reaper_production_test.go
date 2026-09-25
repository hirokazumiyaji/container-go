package container

import (
	"context"
	"strings"
	"testing"
	"time"
)

type productionReaperRunner struct {
	*fakeRunner
	binary string
	uid    string
}

func (r *productionReaperRunner) External() bool         { return true }
func (r *productionReaperRunner) ExternalBinary() string { return r.binary }
func (r *productionReaperRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "run" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return []byte(r.uid + "\n"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestRunRegistersDockerImmutableIDWithProductionReaper(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, logPath := writeReaperStub(t)
	uid := strings.Repeat("ab", 32)
	base := newTestRunner()
	base.imagePresent = true
	runner := &productionReaperRunner{fakeRunner: base, binary: bin, uid: uid}

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("docker-reaper-production"), withRunner(runner), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	globalReapersMu.Lock()
	reaper := globalReapers[bin]
	globalReapersMu.Unlock()
	if reaper == nil {
		t.Fatal("Run did not create the production reaper")
	}
	reaper.mu.Lock()
	entries := append([]reaperEntry(nil), reaper.entries...)
	reaper.mu.Unlock()
	if len(entries) != 1 || entries[0].id != uid {
		t.Fatalf("reaper entries = %+v, want Docker ID %q", entries, uid)
	}

	reaper.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reaper.mu.Lock()
		exited := reaper.exited
		reaper.mu.Unlock()
		if exited != nil {
			select {
			case <-exited:
				globalReapersMu.Lock()
				delete(globalReapers, bin)
				globalReapersMu.Unlock()
				return
			default:
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("production reaper did not exit after EOF")
}
