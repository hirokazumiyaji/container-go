//go:build integration

package examples

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	container "github.com/hirokazumiyaji/container-go"
)

// TestExampleClientAndReuse shows a pinned Client and a reused container:
// two Run calls with the same name and settings return the same container.
func TestExampleClientAndReuse(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	client, err := container.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cg-example-reuse-%d", os.Getpid())

	first, err := client.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithReuse(),
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup skips reused handles, so remove this one explicitly even if a
	// later assertion fails.
	t.Cleanup(func() {
		if err := first.Terminate(context.Background()); err != nil {
			t.Logf("terminate reused container: %v", err)
		}
	})

	second, err := client.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithReuse(),
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("reuse attached to a different container: %q != %q", first.ID(), second.ID())
	}
	// Cleanup never deletes a reused container, so remove it explicitly.
	if err := second.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestExampleCopyWithFiles combines a staged file with copy-in, copy-out, and
// a wait on a one-shot container.
func TestExampleCopyWithFiles(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "motd.txt")
	if err := os.WriteFile(hostFile, []byte("hello from the host\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithCmd("sleep", "60"),
		container.WithFiles(container.FileFromHost(hostFile, "/etc/motd")),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatal(err)
	}

	if err := ctr.CopyToContainer(ctx, hostDir, "/staged"); err != nil {
		t.Fatal(err)
	}
	exit, out, err := ctr.Exec(ctx, []string{"cat", "/staged/motd.txt"})
	if err != nil {
		t.Fatal(err)
	}
	output := readExampleOutput(t, out)
	if exit != 0 || !strings.Contains(output, "hello from the host") {
		t.Fatalf("exec exit=%d output=%q", exit, output)
	}

	reader, err := ctr.CopyFileFromContainer(ctx, "/etc/motd")
	switch {
	case errors.Is(err, container.ErrCopyFileFromContainerUnsupported):
		t.Log("copy-out is unsupported by this backend or host")
	case err != nil:
		t.Fatal(err)
	default:
		defer reader.Close()
		if copied := readExampleOutput(t, reader); !strings.Contains(copied, "hello from the host") {
			t.Fatalf("copied file = %q", copied)
		}
	}

	oneShot, err := container.Run(ctx, integrationAlpine,
		container.WithCmd("true"),
	)
	container.Cleanup(t, oneShot)
	if err != nil {
		t.Fatal(err)
	}
	code, err := oneShot.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("one-shot exit code = %d", code)
	}
}

func readExampleOutput(t *testing.T, r io.Reader) string {
	t.Helper()
	if r == nil {
		return ""
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
