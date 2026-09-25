package container

import (
	"os"
	"strings"
	"testing"
	"time"
)

func waitForLogLines(t *testing.T, path string, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
