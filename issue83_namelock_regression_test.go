//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReviewNameLockUsesHistoricalBarrierAndStableAccountPath(t *testing.T) {
	name := "review-lock-" + newContainerName()
	legacy, err := legacyNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLockFile(context.Background(), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = lockName(ctx, name)
	cancel()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new lock with legacy holder = %v, want deadline", err)
	}

	first, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	second, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("durable path changed with process environment: %q != %q", first, second)
	}
}

func TestReviewReaperHoldsAllNameBarriers(t *testing.T) {
	name := "review-reaper-lock-" + newContainerName()
	const creation = "0123456789abcdef"
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	bin := dir + "/container"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\"'; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLockFile(context.Background(), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register(name, creation); err != nil {
		unlock()
		t.Fatal(err)
	}
	r.closeStdin()
	time.Sleep(150 * time.Millisecond)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete") {
		unlock()
		t.Fatalf("reaper deleted while legacy barrier was held: %s", data)
	}
	unlock()
	waitForLogLines(t, logPath, "delete --force "+name)
}

func TestReviewNameLockRevalidatesOpenedInode(t *testing.T) {
	name := "review-inode-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openNameLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, nameLockFilePerm); err != nil {
		t.Fatal(err)
	}
	if err := checkOpenedLockFile(f, path); err == nil || !strings.Contains(err.Error(), "replaced while opening") {
		t.Fatalf("opened inode replacement error = %v", err)
	}
}
