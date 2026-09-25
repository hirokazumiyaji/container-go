package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cpRunner materializes files for container-to-host copies.
type cpRunner struct {
	*fakeRunner
	fileContent string
}

func (c *cpRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "cp" {
		c.calls = append(c.calls, args)
		if c.failPrefix == "cp" {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
		}
		dst := args[2]
		if !strings.Contains(dst, ":") {
			if err := os.WriteFile(dst, []byte(c.fileContent), 0o600); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil
	}
	return c.fakeRunner.Run(ctx, args...)
}

func TestCopyToContainerBuildsCpArgs(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	src := filepath.Join(t.TempDir(), "init.sql")
	if err := os.WriteFile(src, []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ctr.CopyToContainer(context.Background(), src, "/docker-entrypoint-initdb.d/init.sql"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	call := f.callWith("cp")
	if call == nil {
		t.Fatal("no `cp` call recorded")
	}
	if len(call) != 3 || call[0] != "cp" {
		t.Fatalf("cp args = %v", call)
	}
	if call[1] == src {
		t.Fatal("CLI received the mutable source path")
	}
	if !filepath.IsAbs(call[1]) {
		t.Fatalf("staged source path is not absolute: %q", call[1])
	}
	if got := filepath.Base(call[1]); got != filepath.Base(src) {
		t.Errorf("staged source basename = %q, want %q", got, filepath.Base(src))
	}
	if got, want := call[2], "myctr:/docker-entrypoint-initdb.d/init.sql"; got != want {
		t.Errorf("cp destination = %q, want %q", got, want)
	}
}

func TestCopyToContainerRejectsRelativeContainerPath(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(context.Background(), src, "relative/path"); err == nil {
		t.Fatal("want error for relative container path")
	}
}

func TestCopyToContainerRejectsMissingHostPath(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	err := ctr.CopyToContainer(context.Background(), filepath.Join(t.TempDir(), "nope"), "/x")
	if err == nil {
		t.Fatal("want error for missing host path")
	}
}

func TestCopyFileFromContainerReadsAndCleansUp(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner(), fileContent: "result data"}
	ctr := runTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "result data" {
		t.Errorf("content = %q", data)
	}

	call := f.callWith("cp")
	if call[1] != "myctr:/out/result.txt" {
		t.Errorf("cp src = %q", call[1])
	}
	tmpPath := call[2]
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp copy %q still exists after Close", tmpPath)
	}
}

func TestWithFilesCopiesAfterStart(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	src := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(src, []byte("CREATE TABLE t();"), 0o600); err != nil {
		t.Fatal(err)
	}

	runTestContainer(t, f, WithFiles(File{HostPath: src, ContainerPath: "/init/schema.sql"}))

	call := f.callWith("cp")
	if call == nil || call[2] != "myctr:/init/schema.sql" {
		t.Errorf("cp call = %v", call)
	}
}

func TestWithFilesFailureRollsBack(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	f.failPrefix = "cp"
	src := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithFiles(File{HostPath: src, ContainerPath: "/x"}))
	if err == nil {
		t.Fatal("want error when file copy fails")
	}
	if f.callWith("delete") == nil {
		t.Error("rollback delete not issued")
	}
}

func TestCopyFileFromContainerRejectsRootAndDirectory(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	for _, path := range []string{"/", "/etc/", "/foo/bar/"} {
		if _, err := ctr.CopyFileFromContainer(context.Background(), path); err == nil {
			t.Errorf("path %q: want error for root or directory path", path)
		}
	}
}

type mutatingCopyRunner struct {
	*fakeRunner
	source     string
	copiedPath string
	copied     []byte
}

func (r *mutatingCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "cp" && strings.Contains(args[2], ":") {
		r.copiedPath = args[1]
		if err := os.WriteFile(r.source, []byte("attacker data"), 0o600); err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return nil, nil, err
		}
		r.copied = data
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestCopyToContainerSnapshotsSourceBeforeCLI(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("trusted data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &mutatingCopyRunner{fakeRunner: newTestRunner(), source: source}
	ctr := runTestContainer(t, f)

	if err := ctr.CopyToContainer(context.Background(), source, "/tmp/input.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if string(f.copied) != "trusted data" {
		t.Fatalf("copied data = %q, want trusted data", f.copied)
	}
	if f.copiedPath == source {
		t.Fatal("CLI received the mutable source path")
	}
	if _, err := os.Stat(f.copiedPath); !os.IsNotExist(err) {
		t.Errorf("staged source %q still exists after copy: %v", f.copiedPath, err)
	}
}

type directoryCopyRunner struct {
	*fakeRunner
	copiedPath string
	isDir      bool
	contents   []byte
}

func (r *directoryCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "cp" && strings.Contains(args[2], ":") {
		r.copiedPath = args[1]
		info, err := os.Stat(r.copiedPath)
		if err != nil {
			return nil, nil, err
		}
		r.isDir = info.IsDir()
		r.contents, err = os.ReadFile(filepath.Join(r.copiedPath, "nested", "data.txt"))
		if err != nil {
			return nil, nil, err
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestCopyToContainerPreservesDirectorySemantics(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data.txt"), []byte("tree data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &directoryCopyRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if err := ctr.CopyToContainer(context.Background(), source, "/tmp/tree"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if f.copiedPath == source {
		t.Fatal("CLI received the mutable directory path")
	}
	if got := filepath.Base(f.copiedPath); got != filepath.Base(source) {
		t.Fatalf("staged directory basename = %q, want %q", got, filepath.Base(source))
	}
	if !f.isDir {
		t.Fatal("staged source is not a directory")
	}
	if string(f.contents) != "tree data" {
		t.Errorf("copied directory contents = %q, want tree data", f.contents)
	}
}

func TestCopyToContainerKeepsStagingOutsideSource(t *testing.T) {
	source := t.TempDir()
	t.Setenv("TMPDIR", source)
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	if err := ctr.CopyToContainer(context.Background(), source, "/tmp/tree"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	call := runner.callWith("cp")
	if call == nil || call[1] == source {
		t.Fatalf("cp source = %v, want a staging path outside %q", call, source)
	}
}

func TestCopyToContainerRejectsSymlinkSource(t *testing.T) {
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	err := ctr.CopyToContainer(context.Background(), link, "/tmp/secret")
	if err == nil {
		t.Fatal("want error for symlink source")
	}
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Errorf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if f.callWith("cp") != nil {
		t.Error("CLI was called for a symlink source")
	}
}

func TestCopyToContainerRejectsSymlinkInDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(source, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	err := ctr.CopyToContainer(context.Background(), source, "/tmp/tree")
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called for a directory containing a symlink")
	}
}

func TestCopyToContainerRejectsOversizedSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxCopyToContainerSize + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	err = ctr.CopyToContainer(context.Background(), source, "/tmp/large")
	if !errors.Is(err, ErrCopySourceTooLarge) {
		t.Fatalf("error = %v, want ErrCopySourceTooLarge", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called for an oversized source")
	}
}
