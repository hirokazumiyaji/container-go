package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestDockerNotFoundSurvivesCarriageReturnProgress(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 8000 ]; do printf 'Downloading 50%%\\r' >&2; i=$((i+1)); done\n" +
		"printf 'Error response from daemon: No such container: myctr\\n' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	runner := &cli.ExecRunner{Binary: binary, Identity: "docker"}
	_, _, err := runner.Run(context.Background(), "inspect", "myctr")
	if err == nil {
		t.Fatal("Run returned nil for a failed command")
	}
	if !strings.Contains(err.Error(), "No such container: myctr") {
		t.Fatalf("error lost the not-found diagnostic: %q", tailText(err.Error(), 200))
	}
	if !isNotFound(err) {
		t.Fatalf("error = %v, want a Docker not-found classification", err)
	}
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
