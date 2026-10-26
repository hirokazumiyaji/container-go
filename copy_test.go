package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const defaultTestDockerVersion = `{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.0"}}`

// cpRunner materializes files for container-to-host copies.
type cpRunner struct {
	*fakeRunner
	fileContent   string
	materialize   func(dst string) error
	containerID   string
	versionOutput string
	versionErr    error
}

func (c *cpRunner) isContainerSpec(arg string) bool {
	id := c.containerID
	if id == "" {
		id = strings.Repeat("a", 64)
	}
	return strings.HasPrefix(arg, id+":/")
}

func (c *cpRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "version" {
		c.mu.Lock()
		c.calls = append(c.calls, args)
		c.mu.Unlock()
		if c.versionErr != nil {
			return nil, nil, c.versionErr
		}
		output := c.versionOutput
		if output == "" {
			output = defaultTestDockerVersion
		}
		return []byte(output), nil, nil
	}
	if args[0] == "cp" {
		c.mu.Lock()
		c.calls = append(c.calls, args)
		c.mu.Unlock()
		if c.failPrefix == "cp" {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
		}
		// A container spec (id:/path) is not a host path. Match the
		// known container ID rather than looking for a colon, since a
		// Windows host path can also contain a drive-letter colon.
		if c.isContainerSpec(args[1]) {
			dst := args[2]
			if c.materialize != nil {
				if err := c.materialize(dst); err != nil {
					return nil, nil, err
				}
			} else if err := os.WriteFile(dst, []byte(c.fileContent), 0o600); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil
	}
	return c.fakeRunner.Run(ctx, args...)
}

func (c *cpRunner) setBinary(b string) {
	if c != nil && c.fakeRunner != nil {
		c.binary = b
	}
}

func runCopyDockerTestContainer(t *testing.T, f cli.Runner, opts ...Option) *Container {
	t.Helper()
	if cr, ok := f.(interface{ setBinary(string) }); ok {
		cr.setBinary("docker")
	}
	if fr, ok := f.(*fakeRunner); ok {
		fr.binary = "docker"
	}
	return runTestContainer(t, f, append([]Option{withEngine(dockerEngine{})}, opts...)...)
}

func skipIfCopyFileOpenUnsupported(t *testing.T) {
	t.Helper()
	if err := checkCopyFileOpenCapability(); err != nil {
		t.Skipf("copy-out is not supported on this host: %v", err)
	}
}

// copyStagingTestRoot redirects copy staging to a per-test cache base on
// platforms that honor an environment override and returns the verified
// staging root either way.
func copyStagingTestRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "darwin" {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
	} else if runtime.GOOS != "windows" {
		// LocalAppData on Windows is resolved through KnownFolderPath,
		// which ignores environment overrides.
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
	}
	root, err := prepareCopyStagingRoot()
	if err != nil {
		t.Fatalf("prepareCopyStagingRoot: %v", err)
	}
	return root
}

func copyStagingEntryCount(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read staging root %q: %v", root, err)
	}
	return len(entries)
}

func TestCopyFileFromContainerRejectsAppleBackendBeforeCLI(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner(), fileContent: "must not be copied"}
	ctr := runTestContainer(t, f)

	_, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}
	if call := f.callWith("cp"); call != nil {
		t.Fatalf("Apple copy-out invoked CLI: %v", call)
	}
}

func TestCopyFileFromContainerRejectsUnsupportedDockerVersionBeforeCopy(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	cases := []struct {
		name   string
		output string
		err    error
	}{
		{
			name:   "client",
			output: `{"Client":{"Version":"29.6.9"},"Server":{"Version":"29.7.0"}}`,
		},
		{
			name:   "server",
			output: `{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.6.9"}}`,
		},
		{
			name:   "client development suffix",
			output: `{"Client":{"Version":"29.7.0-dev"},"Server":{"Version":"29.7.0"}}`,
		},
		{
			name:   "server git describe suffix",
			output: `{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.0-10-gdeadbee"}}`,
		},
		{
			name:   "client numeric suffix",
			output: `{"Client":{"Version":"29.7.0-0.1"},"Server":{"Version":"29.7.0"}}`,
		},
		{
			name:   "server empty suffix",
			output: `{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.0-"}}`,
		},
		{
			name:   "client unknown build metadata",
			output: `{"Client":{"Version":"29.7.0+garbage"},"Server":{"Version":"29.7.0"}}`,
		},
		{
			name:   "malformed",
			output: `{"Client":{"Version":"29.7.0"}}`,
		},
		{
			name: "probe failure",
			err:  errors.New("version unavailable"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyStagingTestRoot(t)
			before := copyStagingEntryCount(t, root)

			f := &cpRunner{
				fakeRunner:    newTestRunner(),
				versionOutput: tc.output,
				versionErr:    tc.err,
			}
			ctr := runCopyDockerTestContainer(t, f)

			rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
			if rc != nil {
				_ = rc.Close()
				t.Fatal("unsupported Docker version returned a reader")
			}
			if !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
				t.Fatalf("error = %v, want ErrCopyFileFromContainerUnsupported", err)
			}
			if call := f.callWith("cp"); call != nil {
				t.Fatalf("unsupported Docker version invoked copy: %v", call)
			}
			if call := f.callWith("version"); call == nil {
				t.Fatal("Docker version was not checked")
			}
			if after := copyStagingEntryCount(t, root); after != before {
				t.Fatalf("unsupported Docker version left staging entries: %d before, %d after", before, after)
			}
		})
	}
}

func TestCopyFileFromContainerAcceptsMinimumDockerVersion(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	f := &cpRunner{
		fakeRunner:    newTestRunner(),
		fileContent:   "minimum version",
		versionOutput: `{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.0"}}`,
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "minimum version" {
		t.Errorf("copied content = %q", data)
	}
}

func TestCpRunnerRecognizesDriveLikeContainerID(t *testing.T) {
	f := &cpRunner{
		fakeRunner:  newTestRunner(),
		containerID: "C",
		fileContent: "copied",
	}
	dst := filepath.Join(t.TempDir(), "payload")
	if _, _, err := f.Run(context.Background(), "cp", "C:/out/file", dst); err != nil {
		t.Fatalf("cp: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "copied" {
		t.Errorf("copied content = %q", data)
	}
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

func TestValidateContainerPathRejectsBackslash(t *testing.T) {
	for _, p := range []string{"/out/literal\\name", "/out/dir\\..\\secret"} {
		err := validateContainerPath(p)
		if err == nil || !strings.Contains(err.Error(), "path separator") {
			t.Errorf("validateContainerPath(%q) = %v, want separator error", p, err)
		}
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
	skipIfCopyFileOpenUnsupported(t)
	f := &cpRunner{fakeRunner: newTestRunner(), fileContent: "result data"}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "result data" {
		t.Errorf("content = %q", data)
	}

	call := f.callWith("cp")
	if call[1] != strings.Repeat("a", 64)+":/out/result.txt" {
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
	ctr := runCopyDockerTestContainer(t, f)

	for _, path := range []string{"/", "/etc/", "/foo/bar/"} {
		if _, err := ctr.CopyFileFromContainer(context.Background(), path); err == nil {
			t.Errorf("path %q: want error for root or directory path", path)
		}
	}
}

func TestCopyFileFromContainerUsesCleanPOSIXPathAndFixedDestination(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	root := copyStagingTestRoot(t)

	uid := strings.Repeat("a", 64)
	f := &cpRunner{fakeRunner: newTestRunner(), fileContent: "safe", containerID: uid}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/a/b/..")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	defer rc.Close()

	call := f.callWith("cp")
	if call == nil {
		t.Fatal("cp call not recorded")
	}
	if call[1] != uid+":/a" {
		t.Errorf("cp source = %q, want cleaned POSIX path", call[1])
	}
	dst := call[2]
	if filepath.Base(dst) != "payload" {
		t.Errorf("copy destination = %q, want fixed payload name", dst)
	}
	rel, err := filepath.Rel(root, dst)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("copy destination %q escaped temp root %q", dst, root)
	}
	if filepath.Dir(dst) == root {
		t.Errorf("copy destination %q is not inside a private directory", dst)
	}
}

func TestCopyFileFromContainerRejectsSymlinkWithoutReadingTarget(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "host-secret")
	if err := os.WriteFile(secret, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	var linkErr error
	f := &cpRunner{
		fakeRunner:  newTestRunner(),
		containerID: strings.Repeat("a", 64),
		materialize: func(dst string) error {
			linkErr = os.Symlink(secret, dst)
			return linkErr
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/container/link")
	if linkErr != nil {
		if rc != nil {
			_ = rc.Close()
		}
		t.Skipf("symlink creation unavailable: %v", linkErr)
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatal("symlink target returned a reader")
	}
	if err == nil {
		t.Fatal("symlink target was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("symlink error = %v, want ErrCopyFileNotRegular", err)
	}
}

func TestCopyFileFromContainerRejectsCopiedDirectory(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	f := &cpRunner{
		fakeRunner:  newTestRunner(),
		containerID: strings.Repeat("a", 64),
		materialize: func(dst string) error {
			return os.Mkdir(dst, 0o700)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	_, err := ctr.CopyFileFromContainer(context.Background(), "/container/dir")
	if err == nil {
		t.Fatal("copied directory was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("directory error = %v, want ErrCopyFileNotRegular", err)
	}
}

func TestCopyFileFromContainerRejectsOversizedFileAndCleansUp(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	root := copyStagingTestRoot(t)
	before := copyStagingEntryCount(t, root)

	var dst string
	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(path string) error {
			dst = path
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if err := file.Truncate(MaxCopyFileFromContainerSize + 1); err != nil {
				_ = file.Close()
				return err
			}
			return file.Close()
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/large")
	if rc != nil {
		_ = rc.Close()
		t.Fatal("oversized file returned a reader")
	}
	if !errors.Is(err, ErrCopyFileFromContainerTooLarge) {
		t.Fatalf("error = %v, want ErrCopyFileFromContainerTooLarge", err)
	}
	if dst == "" {
		t.Fatal("copy destination was not recorded")
	}
	if _, statErr := os.Lstat(filepath.Dir(dst)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("private staging directory %q remains after size rejection: %v", filepath.Dir(dst), statErr)
	}
	if after := copyStagingEntryCount(t, root); after != before {
		t.Errorf("staging entries = %d, want %d after size rejection", after, before)
	}
}

func TestCopyFileFromContainerCleansReadOnlyExtractedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	skipIfCopyFileOpenUnsupported(t)

	var dst string
	t.Cleanup(func() {
		if dst == "" {
			return
		}
		_ = os.Chmod(filepath.Join(dst, "nested"), 0o700)
		_ = os.Chmod(dst, 0o700)
	})
	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(path string) error {
			dst = path
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			nested := filepath.Join(path, "nested")
			if err := os.Mkdir(nested, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(nested, "data.txt"), []byte("data"), 0o400); err != nil {
				return err
			}
			if err := os.Chmod(nested, 0o500); err != nil {
				return err
			}
			return os.Chmod(path, 0o500)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/container/dir")
	if rc != nil {
		_ = rc.Close()
		t.Fatal("copied directory returned a reader")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Fatalf("error = %v, want ErrCopyFileNotRegular", err)
	}
	if dst == "" {
		t.Fatal("copy destination was not recorded")
	}
	if _, statErr := os.Lstat(filepath.Dir(dst)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("read-only private staging directory %q remains after rejection: %v", filepath.Dir(dst), statErr)
	}
}

type copyOutFailureRunner struct {
	*cpRunner
	target string
}

func (r *copyOutFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "cp" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{
			Binary:   "docker",
			Args:     args,
			ExitCode: 1,
			Stderr:   "Error response from daemon: No such container: " + r.target,
		}
	}
	return r.cpRunner.Run(ctx, args...)
}

func TestCopyFileFromContainerClassifiesAndRedactsCpFailure(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	runner := &copyOutFailureRunner{cpRunner: &cpRunner{fakeRunner: newTestRunner()}}
	ctr := runCopyDockerTestContainer(t, runner)
	target := ctr.operationTarget()
	runner.target = target

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/missing")
	if rc != nil {
		_ = rc.Close()
		t.Fatal("failed copy returned a reader")
	}
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	if strings.Contains(err.Error(), target) {
		t.Errorf("error %q exposes the immutable handle %q", err, target)
	}
	call := runner.callWith("cp")
	if call == nil {
		t.Fatal("cp call not recorded")
	}
	if _, statErr := os.Lstat(filepath.Dir(call[2])); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("private staging directory %q remains after cp failure: %v", filepath.Dir(call[2]), statErr)
	}
}

// dockerCopyRunner serves Docker-shaped responses and materializes the host
// side of a container-to-host cp so CopyFileFromContainer can read it back.
// The container UID is the one `docker run --detach` printed, so the handle
// is bound to a real full container ID rather than a hand-injected one.
type dockerCopyRunner struct {
	*dockerRunner
	fileContent string
}

func (c *dockerCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "cp" {
		c.calls = append(c.calls, args)
		// Materialize only for container→host copies. Host destinations on
		// Windows contain a drive-letter colon (C:\...), so checking the
		// destination for ":" would skip writing the payload entirely.
		// Container paths are POSIX and always appear as id:/path.
		if strings.Contains(args[1], ":/") {
			if err := os.WriteFile(args[2], []byte(c.fileContent), 0o600); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil
	}
	return c.dockerRunner.Run(ctx, args...)
}

func newDockerCopyRunner(t *testing.T, fileContent string) *dockerCopyRunner {
	t.Helper()
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	return &dockerCopyRunner{
		dockerRunner: &dockerRunner{fakeRunner: newTestRunner(), inspectJSON: data},
		fileContent:  fileContent,
	}
}

// runDockerCopyContainer starts a real container with r and returns the
// handle, asserting that its immutable UID came from Run's parseRunID
// publication path rather than being injected by the test.
func runDockerCopyContainer(t *testing.T, r cli.Runner) *Container {
	t.Helper()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !dockerIDRE.MatchString(ctr.immutableID()) {
		t.Fatalf("published uid = %q, want a full 64-hex container ID", ctr.immutableID())
	}
	if ctr.immutableID() == ctr.ID() {
		t.Fatal("handle UID is the logical name, not a published immutable ID")
	}
	return ctr
}

func TestDockerCopyUsesPublishedImmutableContainerID(t *testing.T) {
	r := newDockerCopyRunner(t, "result data")
	ctr := runDockerCopyContainer(t, r)
	uid := ctr.immutableID()
	if uid == "myctr" {
		t.Fatal("copy target is the logical name, not the published immutable ID")
	}

	src := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ctr.CopyToContainer(context.Background(), src, "/tmp/input.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	call := r.callWith("cp")
	if len(call) != 3 || call[0] != "cp" || call[1] == src || call[2] != uid+":/tmp/input.txt" {
		t.Fatalf("copy-to args = %v, want staged source and immutable target %q", call, uid)
	}

	r.calls = nil
	rc, err := ctr.CopyFileFromContainer(context.Background(), "/tmp/output.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "result data" {
		t.Errorf("content = %q, want %q", data, "result data")
	}
	call = r.callWith("cp")
	if len(call) != 3 || call[1] != uid+":/tmp/output.txt" {
		t.Fatalf("copy-from args = %v, want immutable target %q", call, uid)
	}
}

// TestDockerCopyIgnoresSameNameReplacement proves the immutable UID is
// binding: a same-name replacement cannot receive the copy, and the stale
// handle never re-resolves the name to the replacement's ID.
func TestDockerCopyIgnoresSameNameReplacement(t *testing.T) {
	const replacementID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := newDockerCopyRunner(t, "replacement bytes")
	ctr := runDockerCopyContainer(t, r)
	uid := ctr.immutableID()

	// The name now resolves to a different container. Copies must still
	// address the UID the handle was published with.
	r.inspectJSON = []byte(`[{"Id":"` + replacementID + `","Name":"/myctr","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{}},"NetworkSettings":{}}]`)
	r.calls = nil

	src := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(context.Background(), src, "/tmp/input.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	call := r.callWith("cp")
	if len(call) != 3 || call[2] != uid+":/tmp/input.txt" {
		t.Fatalf("copy-to wrote to %v, want the bound UID %q", call, uid)
	}
	if strings.Contains(strings.Join(call, " "), replacementID) {
		t.Fatalf("copy-to targeted the same-name replacement: %v", call)
	}

	r.calls = nil
	rc, err := ctr.CopyFileFromContainer(context.Background(), "/tmp/output.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	_ = rc.Close()
	call = r.callWith("cp")
	if len(call) != 3 || call[1] != uid+":/tmp/output.txt" {
		t.Fatalf("copy-from read from %v, want the bound UID %q", call, uid)
	}
	if strings.Contains(strings.Join(call, " "), replacementID) {
		t.Fatalf("copy-from read the same-name replacement: %v", call)
	}
}

func TestDockerCopyFailsClosedWithoutImmutableContainerID(t *testing.T) {
	cases := []struct {
		name string
		uid  string
	}{
		{"missing", ""},
		{"short", "0f1e2d3c"},
		{"nonhex", strings.Repeat("z", 64)},
		{"uppercase", strings.ToUpper(dockerFixtureID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newDockerCopyRunner(t, "")
			ctr := runDockerCopyContainer(t, r)
			// Corrupt only the published identity; the handle, runner, and
			// engine are otherwise exactly what Run returned.
			ctr.pack().uid = tc.uid
			r.calls = nil

			src := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := ctr.CopyToContainer(context.Background(), src, "/tmp/input.txt")
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("copy-to error = %v, want ErrGenerationReplaced", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("copy-to issued CLI calls with uid %q: %v", tc.uid, r.calls)
			}

			if _, err := ctr.CopyFileFromContainer(context.Background(), "/tmp/output.txt"); !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("copy-from error = %v, want ErrGenerationReplaced", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("copy-from issued CLI calls with uid %q: %v", tc.uid, r.calls)
			}
		})
	}
}

// blockingInspectCopyRunner holds the first inspect open so inspectMu stays
// locked across the whole backend call, reproducing a slow daemon.
type blockingInspectCopyRunner struct {
	*dockerCopyRunner
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	blocked  bool
	released bool
}

func (r *blockingInspectCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" && r.blockInspect() {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.dockerCopyRunner.Run(ctx, args...)
}

func (r *blockingInspectCopyRunner) blockInspect() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blocked
}

func (r *blockingInspectCopyRunner) unblock() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	close(r.release)
}

// TestDockerCopyDoesNotWaitBehindBlockedInspect is the regression for the
// cancellable identity lock: a copy must honor its own context even while
// another caller holds inspectMu inside a long inspect.
func TestDockerCopyDoesNotWaitBehindBlockedInspect(t *testing.T) {
	r := &blockingInspectCopyRunner{
		dockerCopyRunner: newDockerCopyRunner(t, "data"),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	ctr := runDockerCopyContainer(t, r)
	// Only now block, so Run's own setup is unaffected.
	r.mu.Lock()
	r.blocked = true
	r.mu.Unlock()
	defer r.unblock()

	inspectDone := make(chan error, 1)
	go func() {
		_, err := ctr.State(context.Background())
		inspectDone <- err
	}()

	src := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{"copy-to", func(ctx context.Context) error {
			return ctr.CopyToContainer(ctx, src, "/tmp/input.txt")
		}},
		{"copy-from", func(ctx context.Context) error {
			rc, err := ctr.CopyFileFromContainer(ctx, "/tmp/output.txt")
			if rc != nil {
				_ = rc.Close()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled copy waited behind the blocked inspect lock")
			}
		})
	}

	r.unblock()
	if err := <-inspectDone; err != nil {
		t.Fatalf("State: %v", err)
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
func TestWithFilesRejectsDirectoryBeforeBackend(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := newCountingRunner(newTestRunner())

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: source, ContainerPath: "/tmp/tree"}))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Option != "WithFiles" || validationErr.Field != "hostPath" {
		t.Fatalf("error = %v, want ValidationError for WithFiles/hostPath", err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want regular-file rejection", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("backend CLI calls = %d, want 0: %v", got, runner.args)
	}
}

func TestWithFilesRejectsSymlinkBeforeBackend(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, source); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runner := newCountingRunner(newTestRunner())

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: source, ContainerPath: "/tmp/link.txt"}))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Option != "WithFiles" || validationErr.Field != "hostPath" {
		t.Fatalf("error = %v, want ValidationError for WithFiles/hostPath", err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want regular-file rejection", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("backend CLI calls = %d, want 0: %v", got, runner.args)
	}
}

func TestWithFilesAcceptsRegularFile(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newCountingRunner(newTestRunner())

	runTestContainer(t, runner, WithFiles(File{HostPath: source, ContainerPath: "/tmp/input.txt"}))

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.calls.Load() == 0 {
		t.Fatal("backend CLI was not invoked for a regular file")
	}
	for _, args := range runner.args {
		if len(args) == 3 && args[0] == "cp" && args[2] == "myctr:/tmp/input.txt" {
			return
		}
	}
	t.Fatalf("no cp call for the regular file: %v", runner.args)
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
		// Windows denies a replace-over-open-file even when the handle
		// was opened with FILE_SHARE_DELETE. Remove unlinks the name
		// while the snapshot handle keeps the trusted contents.
		if removeErr := os.Remove(path); removeErr != nil {
			return err
		}
		if err := os.Rename(replacement, path); err != nil {
			return err
		}
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
	var dir string
	if runtime.GOOS == "windows" {
		// Walking a file-as-parent is "not exist" on Windows, which cleanup
		// treats as success. An illegal name still produces a walk error.
		dir = filepath.Join(t.TempDir(), "staging*")
	} else {
		parentFile := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(parentFile, "staging")
	}
	if err := cleanupCopyStagingDir(dir); err == nil {
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

func TestWithFilesRejectsDuplicateContainerPath(t *testing.T) {
	tmp := t.TempDir()
	f1 := filepath.Join(tmp, "f1.txt")
	f2 := filepath.Join(tmp, "f2.txt")
	if err := os.WriteFile(f1, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Within single WithFiles
	runner := &cpRunner{fakeRunner: newTestRunner()}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(
			File{HostPath: f1, ContainerPath: "/etc/app.conf"},
			File{HostPath: f2, ContainerPath: "/etc/app.conf"},
		),
	)
	if err == nil {
		t.Fatal("Run succeeded with duplicate container paths in single WithFiles, want error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "WithFiles" || valErr.Field != "containerPath" {
		t.Fatalf("err = %v, want ValidationError for WithFiles/containerPath", err)
	}

	// Across multiple WithFiles
	_, err = Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: f1, ContainerPath: "/etc/app.conf"}),
		WithFiles(File{HostPath: f2, ContainerPath: "/etc/app.conf"}),
	)
	if err == nil {
		t.Fatal("Run succeeded with duplicate container paths across multiple WithFiles, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithFiles" || valErr.Field != "containerPath" {
		t.Fatalf("err = %v, want ValidationError for WithFiles/containerPath", err)
	}
}

func TestWithFilesParallelCopiesMultipleFiles(t *testing.T) {
	tmp := t.TempDir()
	var files []File
	for i := 0; i < 6; i++ {
		p := filepath.Join(tmp, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("content%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, File{HostPath: p, ContainerPath: fmt.Sprintf("/data/file%d.txt", i)})
	}

	runner := &cpRunner{fakeRunner: newTestRunner()}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(files...),
	)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	cpCalls := 0
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "cp" {
			cpCalls++
		}
	}
	if cpCalls != len(files) {
		t.Fatalf("cp calls = %d, want %d", cpCalls, len(files))
	}
}

type concurrentCPRunner struct {
	*fakeRunner
	active    atomic.Int32
	maxActive atomic.Int32
	barrier   chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
}

func (c *concurrentCPRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "cp" {
		cur := c.active.Add(1)
		for {
			oldMax := c.maxActive.Load()
			if cur <= oldMax || c.maxActive.CompareAndSwap(oldMax, cur) {
				break
			}
		}
		c.enterOnce.Do(func() {
			close(c.entered)
		})
		select {
		case <-c.barrier:
		case <-ctx.Done():
			c.active.Add(-1)
			return nil, nil, ctx.Err()
		}
		c.active.Add(-1)
		c.mu.Lock()
		c.calls = append(c.calls, args)
		c.mu.Unlock()
		return nil, nil, nil
	}
	return c.fakeRunner.Run(ctx, args...)
}

func TestWithFilesExecutesConcurrently(t *testing.T) {
	tmp := t.TempDir()
	var files []File
	for i := 0; i < 4; i++ {
		p := filepath.Join(tmp, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, File{HostPath: p, ContainerPath: fmt.Sprintf("/data/file%d.txt", i)})
	}

	runner := &concurrentCPRunner{
		fakeRunner: newTestRunner(),
		barrier:    make(chan struct{}),
		entered:    make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
			WithFiles(files...),
		)
		errCh <- err
	}()

	// Wait until at least one copy enters
	<-runner.entered

	// Give goroutines time to reach the barrier concurrently
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runner.maxActive.Load() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Release all workers
	close(runner.barrier)

	if err := <-errCh; err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if max := runner.maxActive.Load(); max < 2 {
		t.Fatalf("max concurrent cp operations = %d, want >= 2", max)
	}
}

func TestWithFilesParallelFailureRollsBack(t *testing.T) {
	tmp := t.TempDir()
	var files []File
	for i := 0; i < 4; i++ {
		p := filepath.Join(tmp, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, File{HostPath: p, ContainerPath: fmt.Sprintf("/data/file%d.txt", i)})
	}

	runner := &cpRunner{fakeRunner: newTestRunner()}
	runner.failPrefix = "cp"

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(files...),
	)
	if err == nil {
		t.Fatal("Run succeeded despite copy failure, want error")
	}

	if runner.callWith("delete") == nil {
		t.Error("container was not rolled back after copy failure")
	}
}

// TestCopyFilesConcurrentlyReportsCancellationBetweenCopies pins the contract
// that a run cancelled while no worker is inside a copy must not report
// success. Every worker leaves through the pre-copy guard without recording an
// error, so the helper has to surface the context itself: returning nil there
// would claim an incomplete copy set was fully applied and skip the rollback
// the sequential loop used to trigger.
func TestCopyFilesConcurrentlyReportsCancellationBetweenCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	files := make([]File, 0, defaultCopyFilesParallelism+1)
	for i := 0; i < defaultCopyFilesParallelism+1; i++ {
		files = append(files, File{
			HostPath:      fmt.Sprintf("/host/file%d", i),
			ContainerPath: fmt.Sprintf("/data/file%d", i),
		})
	}

	release := make(chan struct{})
	var started, copied atomic.Int32
	err := copyFilesConcurrently(ctx, files, func(context.Context, File) error {
		// Cancel while the first copy is in flight and hold every worker until
		// it is released, so the file still queued is dropped by the guard on
		// the next iteration instead of being copied.
		if started.Add(1) == 1 {
			cancel()
			close(release)
		}
		<-release
		copied.Add(1)
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyFilesConcurrently error = %v, want context.Canceled", err)
	}
	if got, total := copied.Load(), int32(len(files)); got >= total {
		t.Fatalf("copied = %d of %d, want fewer: the test no longer models an incomplete copy set", got, total)
	}
}
