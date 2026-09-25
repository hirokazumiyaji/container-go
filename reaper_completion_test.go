package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewReusePromotionFailureCancelsAlternateIdentity(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r := newReaper("unused", "rm")
	r.closed = true // exercise cancellation bookkeeping without spawning a child
	r.entries = []reaperEntry{
		{id: "reuse-promotion", creation: "aaaaaaaaaaaaaaaa", pending: true},
		{id: uid},
	}
	reg := &reaperRegistration{
		reaper:    r,
		entry:     r.entries[0],
		alternate: reaperEntry{id: uid},
	}
	unregisterWithGlobalReaper(reg)
	r.mu.Lock()
	remaining := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(remaining) != 0 {
		t.Fatalf("reaper entries after failed promotion cleanup = %+v, want none", remaining)
	}
}

func TestReviewReaperCompletionStopsPendingRetry(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nif [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'; echo '  \"" + managedLabel + "\": \"true\"'; echo '  \"" + sessionLabel + "\": \"" + sessionID() + "\"'; echo '  \"state\": \"running\"'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	defer waitReviewReaperExit(r)
	if err := r.registerPendingOwned("complete-review", "0123456789abcdef", sessionID()); err != nil {
		t.Fatal(err)
	}
	if err := r.completePending("complete-review", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force complete-review")
}
