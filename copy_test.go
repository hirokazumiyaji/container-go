package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		c.calls = append(c.calls, args)
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
		c.calls = append(c.calls, args)
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
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			t.Setenv("TMP", root)
			t.Setenv("TEMP", root)

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
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read temp root: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("unsupported Docker version left temp entries: %v", entries)
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
	want := []string{"cp", src, "myctr:/docker-entrypoint-initdb.d/init.sql"}
	if !slices.Equal(call, want) {
		t.Errorf("cp args = %v, want %v", call, want)
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
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)

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
	if !dockerIDRE.MatchString(ctr.uid) {
		t.Fatalf("published uid = %q, want a full 64-hex container ID", ctr.uid)
	}
	if ctr.uid == ctr.ID() {
		t.Fatal("handle UID is the logical name, not a published immutable ID")
	}
	return ctr
}

func TestDockerCopyUsesPublishedImmutableContainerID(t *testing.T) {
	r := newDockerCopyRunner(t, "result data")
	ctr := runDockerCopyContainer(t, r)
	uid := ctr.uid
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
	if !slices.Equal(call, []string{"cp", src, uid + ":/tmp/input.txt"}) {
		t.Fatalf("copy-to args = %v, want immutable target %q", call, uid)
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
	uid := ctr.uid

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
			ctr.uid = tc.uid
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
