package container

import (
	"os"
	"strings"
	"testing"
	"time"
)

func waitForLogLines(t *testing.T, path string, wants ...string) {
	t.Helper()
	waitForReaperLogLinesWithin(t, path, 5*time.Second, wants...)
}

func waitForReaperLogLinesWithin(t *testing.T, path string, timeout time.Duration, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		found := true
		for _, want := range wants {
			if !strings.Contains(string(data), want) {
				found = false
				break
			}
		}
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log %s = %q, want all of %q", path, data, wants)
}

func closeReaperForTest(t *testing.T, r *reaper) {
	t.Helper()
	r.closeStdin()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not exit after stdin close")
	}
}
