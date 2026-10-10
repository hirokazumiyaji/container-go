package cli

import (
	"strings"
	"testing"
)

func TestTruncateOutputRetainsHeadAndDecisiveTail(t *testing.T) {
	tail := "Cannot connect to the Docker daemon"
	input := strings.Repeat("diagnostic prefix\n", 8000) + tail
	got := truncateOutput(input)
	if len(got) > maxStderr {
		t.Fatalf("len(truncateOutput) = %d, want <= %d", len(got), maxStderr)
	}
	if !strings.Contains(got, "diagnostic prefix") {
		t.Fatal("truncateOutput dropped the diagnostic head")
	}
	if !strings.HasSuffix(got, tail) {
		t.Fatalf("truncateOutput tail = %q, want %q", got[len(got)-minInt(len(got), len(tail)):], tail)
	}
}

func TestWithStdoutRetainsDecisiveTail(t *testing.T) {
	tail := "Error response from daemon: no such object: myctr"
	input := strings.Repeat("stdout prefix\n", 8000) + tail
	err := WithStdout(&CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
	}, input)
	stdout, _, ok := DiagnosticText(err)
	if !ok {
		t.Fatal("DiagnosticText did not find CLIError")
	}
	if len(stdout) > maxStderr {
		t.Fatalf("len(stdout) = %d, want <= %d", len(stdout), maxStderr)
	}
	if !strings.Contains(stdout, "stdout prefix") || !strings.HasSuffix(stdout, tail) {
		t.Fatalf("truncated stdout lost head or tail: %q", stdout)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
