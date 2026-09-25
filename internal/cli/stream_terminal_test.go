package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReviewFiniteStreamPropagatesTerminalCLIError(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf out; printf err >&2; exit 7`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(stream)
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("readErr=%v, want CLIError", readErr)
	}
	if cliErr.ExitCode != 7 || !strings.Contains(cliErr.Stderr, "err") {
		t.Fatalf("cliErr=%+v", cliErr)
	}
	if !strings.Contains(string(data), "out") {
		t.Fatalf("data=%q", data)
	}
}
