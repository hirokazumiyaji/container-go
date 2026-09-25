//go:build !windows

package container

import (
	"os"
	"testing"
)

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
