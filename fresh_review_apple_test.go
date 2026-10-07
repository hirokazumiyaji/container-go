//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshReviewAppleReaperHoldsNameLockAcrossReplacement(t *testing.T) {
	dir := t.TempDir()
	tmpDir := filepath.Join(dir, "tmp with spaces")
	if err := os.Mkdir(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpDir)
	logPath := filepath.Join(dir, "calls.log")
	statePath := filepath.Join(dir, "generation")
	binPath := filepath.Join(dir, "container")
	const creation = "0123456789abcdef"
	const replacement = "bbbbbbbbbbbbbbbb"
	if err := os.WriteFile(statePath, []byte(creation), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then sleep 1; current=$(cat " + statePath + "); echo '  \"" + creationLabel + "\": \"'\"$current\"'\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("locked", creation); err != nil {
		t.Fatal(err)
	}

	unlock, err := lockName(context.Background(), "locked")
	if err != nil {
		t.Fatal(err)
	}
	// Replace the generation while the cooperating name lock is held. The
	// reaper must acquire that same lock before inspecting, then observe
	// the replacement and refuse the old generation.
	if err := os.WriteFile(statePath, []byte(replacement), 0o600); err != nil {
		unlock()
		t.Fatal(err)
	}
	r.closeStdin()
	time.Sleep(250 * time.Millisecond)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "inspect locked") || strings.Contains(string(data), "delete --force locked") {
		unlock()
		t.Fatalf("reaper entered name operation while lock was held: %s", data)
	}
	unlock()
	waitForLogLines(t, logPath, "inspect locked")
	time.Sleep(200 * time.Millisecond)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force locked") {
		t.Fatalf("reaper deleted replaced generation: %s", data)
	}
}
