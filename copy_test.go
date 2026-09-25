package container

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestCopyToContainerClassifiesDisappearanceAndPreservesCause(t *testing.T) {
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	err := ctr.CopyToContainer(context.Background(), filepath.Join(t.TempDir(), "gone"), "/tmp/gone")
	if !errors.Is(err, ErrCopySourceChanged) {
		t.Fatalf("error = %v, want ErrCopySourceChanged", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want preserved fs.ErrNotExist cause", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called after the source disappeared")
	}
}

func TestSnapshotCopySourceClassifiesPostOpenDisappearance(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	openers := defaultCopySourceOpeners
	openers.open = func(path string) (*os.File, bool, error) {
		file, reparse, err := openCopySource(path)
		if err != nil {
			return nil, false, err
		}
		if err := os.Remove(path); err != nil {
			_ = file.Close()
			return nil, false, err
		}
		return file, reparse, nil
	}

	_, _, err := snapshotCopySourceWith(context.Background(), source, true, defaultCopySnapshotLimits, openers)
	if !errors.Is(err, ErrCopySourceChanged) {
		t.Fatalf("error = %v, want ErrCopySourceChanged", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want preserved fs.ErrNotExist cause", err)
	}
}

func TestWithFilesRejectsDirectory(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &cpRunner{fakeRunner: newTestRunner()}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: source, ContainerPath: "/tmp/tree"}))
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if call := runner.callWith("cp"); call != nil {
		t.Errorf("CLI was called for a file-only WithFiles source: %v", call)
	}
	if runner.callWith("delete") == nil {
		t.Error("failed WithFiles copy did not roll back the container")
	}
}

func TestCopyToContainerCleansNonEmptyReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	source := filepath.Join(t.TempDir(), "readonly-tree")
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(source, "nested"), 0o700)
		_ = os.Chmod(source, 0o700)
	})
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data.txt"), []byte("tree data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "nested"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o555); err != nil {
		t.Fatal(err)
	}
	runner := &directoryCopyRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	if err := ctr.CopyToContainer(context.Background(), source, "/tmp/tree"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if _, err := os.Lstat(runner.copiedPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only staged tree %q was not removed: %v", runner.copiedPath, err)
	}
}

func TestSnapshotCopySourceUsesOpenedRootAfterSameMetadataReplacement(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	openers := defaultCopySourceOpeners
	openers.open = func(path string) (*os.File, bool, error) {
		file, reparse, err := openCopySource(path)
		if err != nil {
			return nil, false, err
		}
		if err := replaceCopyPathWithSameMetadata(t, file, path); err != nil {
			_ = file.Close()
			return nil, false, err
		}
		return file, reparse, nil
	}

	staged, cleanup, err := snapshotCopySourceWith(context.Background(), source, true, defaultCopySnapshotLimits, openers)
	if err != nil {
		t.Fatalf("snapshotCopySourceWith: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "trusted" {
		t.Fatalf("snapshot opened replacement path: got %q, want trusted", data)
	}
}

func TestSnapshotCopyDirectoryUsesOpenedChildAfterSameMetadataReplacement(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	childPath := filepath.Join(source, "data.txt")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	openers := defaultCopySourceOpeners
	openers.openAt = func(parent *os.File, name string) (*os.File, bool, error) {
		file, reparse, err := openCopySourceAt(parent, name)
		if err != nil {
			return nil, false, err
		}
		if name == "data.txt" {
			if err := replaceCopyPathWithSameMetadata(t, file, childPath); err != nil {
				_ = file.Close()
				return nil, false, err
			}
			if err := os.Chtimes(source, parentInfo.ModTime(), parentInfo.ModTime()); err != nil {
				_ = file.Close()
				return nil, false, err
			}
		}
		return file, reparse, nil
	}

	staged, cleanup, err := snapshotCopySourceWith(context.Background(), source, true, defaultCopySnapshotLimits, openers)
	if err != nil {
		t.Fatalf("snapshotCopySourceWith: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(staged, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "trusted" {
		t.Fatalf("snapshot opened replacement child: got %q, want trusted", data)
	}
}

func replaceCopyPathWithSameMetadata(t *testing.T, opened *os.File, path string) error {
	t.Helper()
	before, err := opened.Stat()
	if err != nil {
		return err
	}
	replacement := path + ".replacement"
	data := strings.Repeat("x", int(before.Size()))
	if err := os.WriteFile(replacement, []byte(data), before.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		return err
	}
	if err := os.Rename(replacement, path); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !sameOpenCopyInfo(before, after) {
		t.Fatalf("replacement metadata = (%d, %v, %v), want (%d, %v, %v)",
			after.Size(), after.Mode(), after.ModTime(), before.Size(), before.Mode(), before.ModTime())
	}
	return nil
}

func TestSnapshotCopySourceRejectsTooManyEmptyEntries(t *testing.T) {
	source := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(source, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := defaultCopySnapshotLimits
	limits.maxEntries = 3 // source root plus two empty entries

	_, _, err := snapshotCopySourceWith(context.Background(), source, true, limits, defaultCopySourceOpeners)
	if !errors.Is(err, ErrCopySourceTooManyEntries) {
		t.Fatalf("error = %v, want ErrCopySourceTooManyEntries", err)
	}
}

func TestSnapshotCopySourceRejectsTreeBeyondDepthLimit(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(source, "one", "two", "three", "four"), 0o700); err != nil {
		t.Fatal(err)
	}
	limits := defaultCopySnapshotLimits
	limits.maxDepth = 3

	_, _, err := snapshotCopySourceWith(context.Background(), source, true, limits, defaultCopySourceOpeners)
	if !errors.Is(err, ErrCopySourceTooDeep) {
		t.Fatalf("error = %v, want ErrCopySourceTooDeep", err)
	}
}

func TestSnapshotCopySourceRejectsMetadataBudget(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "entry"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	limits := defaultCopySnapshotLimits
	limits.maxMetadataBytes = int64(len(source)) // admits only the source root path

	_, _, err := snapshotCopySourceWith(context.Background(), source, true, limits, defaultCopySourceOpeners)
	if !errors.Is(err, ErrCopySourceMetadataTooLarge) {
		t.Fatalf("error = %v, want ErrCopySourceMetadataTooLarge", err)
	}
}

func TestSnapshotCopyDirectoryClassifiesChildDisappearance(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "disappears"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	openers := defaultCopySourceOpeners
	openers.openAt = func(*os.File, string) (*os.File, bool, error) {
		return nil, false, fs.ErrNotExist
	}

	_, _, err := snapshotCopySourceWith(context.Background(), source, true, defaultCopySnapshotLimits, openers)
	if !errors.Is(err, ErrCopySourceChanged) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want ErrCopySourceChanged and fs.ErrNotExist", err)
	}
}

type copyDeadlineRunner struct {
	*fakeRunner
	deadline time.Time
	ctxErr   error
}

func (r *copyDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "cp" && strings.Contains(args[2], ":") {
		r.deadline, _ = ctx.Deadline()
		r.ctxErr = ctx.Err()
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestCleanupCopyStagingDirPropagatesFailure(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupCopyStagingDir(filepath.Join(parentFile, "staging")); err == nil {
		t.Fatal("cleanup error was discarded")
	}
}

func TestCopyToContainerUsesSeparateStagingAndCLIBudgets(t *testing.T) {
	oldTimeout := copyStagingTimeout
	copyStagingTimeout = 5 * time.Second
	defer func() { copyStagingTimeout = oldTimeout }()

	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &copyDeadlineRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)
	started := time.Now()

	if err := ctr.CopyToContainer(context.Background(), source, "/tmp/input.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if runner.ctxErr != nil {
		t.Fatalf("CLI context error = %v", runner.ctxErr)
	}
	if remaining := runner.deadline.Sub(started); remaining < queryTimeout/2 {
		t.Fatalf("CLI deadline has only %v remaining; staging reused its %v budget", remaining, copyStagingTimeout)
	}
}

func TestCopyToContainerPreservesCallerDeadlineForCLI(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantDeadline, _ := ctx.Deadline()
	runner := &copyDeadlineRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	if err := ctr.CopyToContainer(ctx, source, "/tmp/input.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if !runner.deadline.Equal(wantDeadline) {
		t.Fatalf("CLI deadline = %v, want caller deadline %v", runner.deadline, wantDeadline)
	}
}
