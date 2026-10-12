//go:build !windows

package container

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestReaperSkipsReplacedGeneration(t *testing.T) {
	// Stub binary: inspect prints the *current* creation, delete logs.
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/ctr"
	currentCreation := "dddddddddddddddd"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo \"" + currentCreation + "\"; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	oldCreation := "eeeeeeeeeeeeeeee"
	if err := r.register("myctr", oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	// Give the reaper a moment to run; it must NOT delete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 && strings.Contains(string(data), "myctr") {
		t.Fatalf("reaper deleted replaced container: %q", data)
	}
}

func TestReaperDeletesMatchingGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/ctr"
	creation := "ffffffffffffffff"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\",'; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("myctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force myctr")
}
