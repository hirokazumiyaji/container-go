//go:build integration

package container_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

func TestIntegrationDockerWithFilesDirectoryCopiesContents(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	source := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "001_init.sql"), []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "002_more.sql"), []byte("select 2;"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctr, err := container.Run(ctx, integrationRedis,
		container.WithFiles(container.File{HostPath: source, ContainerPath: "/tmp"}))
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertContainerFile(t, ctx, ctr, "/tmp/001_init.sql", "select 1;")
	assertContainerFile(t, ctx, ctr, "/tmp/nested/002_more.sql", "select 2;")
	assertContainerPathAbsent(t, ctx, ctr, "/tmp/"+filepath.Base(source))
}

func TestIntegrationDockerWithFilesDirectoryReuseAttachDoesNotNest(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	source := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "001_init.sql"), []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("containergo-withfiles-%d", time.Now().UnixNano())
	opts := []container.Option{
		container.WithName(name),
		container.WithReuse(),
		container.WithFiles(container.File{HostPath: source, ContainerPath: "/tmp/migrations"}),
	}
	first, err := container.Run(ctx, integrationRedis, opts...)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Terminate(context.Background())
	})
	assertContainerFile(t, ctx, first, "/tmp/migrations/001_init.sql", "select 1;")

	second, err := container.Run(ctx, integrationRedis, opts...)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	assertContainerFile(t, ctx, second, "/tmp/migrations/001_init.sql", "select 1;")
	assertContainerPathAbsent(t, ctx, second, "/tmp/migrations/"+filepath.Base(source))
}

func assertContainerFile(t *testing.T, ctx context.Context, ctr *container.Container, path, want string) {
	t.Helper()
	code, out, err := ctr.Exec(ctx, []string{"cat", path})
	if err != nil {
		t.Fatalf("Exec cat %s: %v", path, err)
	}
	data, readErr := io.ReadAll(out)
	if readErr != nil {
		t.Fatalf("read cat %s: %v", path, readErr)
	}
	if code != 0 || string(data) != want {
		t.Fatalf("cat %s = (code %d, %q), want %q", path, code, data, want)
	}
}

func assertContainerPathAbsent(t *testing.T, ctx context.Context, ctr *container.Container, path string) {
	t.Helper()
	code, out, err := ctr.Exec(ctx, []string{"sh", "-c", "test ! -e " + path})
	if err != nil {
		t.Fatalf("Exec test ! -e %s: %v", path, err)
	}
	data, readErr := io.ReadAll(out)
	if readErr != nil {
		t.Fatalf("read test ! -e %s: %v", path, readErr)
	}
	if code != 0 {
		t.Fatalf("path %s unexpectedly exists (code %d, output %q)", path, code, strings.TrimSpace(string(data)))
	}
}

func TestIntegrationAppleWithFilesDirectoryCopiesContents(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	source := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "001_init.sql"), []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "002_more.sql"), []byte("select 2;"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctr, err := container.Run(ctx, integrationRedis,
		container.WithFiles(container.File{HostPath: source, ContainerPath: "/tmp"}))
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertContainerFile(t, ctx, ctr, "/tmp/001_init.sql", "select 1;")
	assertContainerFile(t, ctx, ctr, "/tmp/nested/002_more.sql", "select 2;")
	assertContainerPathAbsent(t, ctx, ctr, "/tmp/"+filepath.Base(source))
}

func TestIntegrationAppleWithFilesDirectoryReuseAttachDoesNotNest(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	source := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "001_init.sql"), []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("containergo-withfiles-%d", time.Now().UnixNano())
	opts := []container.Option{
		container.WithName(name),
		container.WithReuse(),
		container.WithFiles(container.File{HostPath: source, ContainerPath: "/tmp/migrations"}),
	}
	first, err := container.Run(ctx, integrationRedis, opts...)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Terminate(context.Background())
	})
	assertContainerFile(t, ctx, first, "/tmp/migrations/001_init.sql", "select 1;")

	second, err := container.Run(ctx, integrationRedis, opts...)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	assertContainerFile(t, ctx, second, "/tmp/migrations/001_init.sql", "select 1;")
	assertContainerPathAbsent(t, ctx, second, "/tmp/migrations/"+filepath.Base(source))
}
