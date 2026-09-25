package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

func TestBoundedEnvContextCapsLongerCallerDeadline(t *testing.T) {
	oldTimeout := envFileSecurityTimeout
	envFileSecurityTimeout = 20 * time.Millisecond
	t.Cleanup(func() { envFileSecurityTimeout = oldTimeout })

	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	ctx, cancel := boundedEnvContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("boundedEnvContext returned a context without a deadline")
	}
	if remaining := time.Until(deadline); remaining > 100*time.Millisecond {
		t.Fatalf("bounded deadline = %v, want the shorter security timeout", remaining)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("bounded context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("bounded context did not honor the security timeout")
	}
}

func TestBoundedEnvContextPreservesEarlierCallerDeadline(t *testing.T) {
	oldTimeout := envFileSecurityTimeout
	envFileSecurityTimeout = time.Second
	t.Cleanup(func() { envFileSecurityTimeout = oldTimeout })

	parent, cancelParent := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelParent()
	ctx, cancel := boundedEnvContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("boundedEnvContext returned a context without a deadline")
	}
	if remaining := time.Until(deadline); remaining > 100*time.Millisecond {
		t.Fatalf("bounded deadline = %v, want the earlier caller deadline", remaining)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("bounded context error = %v, want caller deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("bounded context did not honor the earlier caller deadline")
	}
}

func TestEnvValidationRejectsInvalidEntriesBeforeBackendCall(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty key", key: "", value: "value"},
		{name: "equals key", key: "A=B", value: "value"},
		{name: "space key", key: "BAD KEY", value: "value"},
		{name: "Unicode whitespace key", key: "BAD\u00a0KEY", value: "value"},
		{name: "tab key", key: "BAD\tKEY", value: "value"},
		{name: "carriage return key", key: "BAD\rKEY", value: "value"},
		{name: "newline key", key: "BAD\nKEY", value: "value"},
		{name: "NUL key", key: "BAD\x00KEY", value: "value"},
		{name: "comment key", key: "#X", value: "1"},
		{name: "BOM key", key: "\uFEFFX", value: "1"},
		{name: "control key", key: "BAD\x01KEY", value: "value"},
		{name: "invalid UTF-8 key", key: string([]byte{'B', 'A', 'D', 0xff}), value: "value"},
		{name: "invalid UTF-8 value", key: "VALID", value: string([]byte{0xff})},
		{name: "NUL value", key: "VALID", value: "secret\x00"},
		{name: "newline value", key: "VALID", value: "line1\nline2"},
		{name: "carriage return value", key: "VALID", value: "line1\rline2"},
		{name: "Unicode line separator value", key: "VALID", value: "line1\u2028line2"},
		{name: "Unicode paragraph separator value", key: "VALID", value: "line1\u2029line2"},
		{name: "C1 control value", key: "VALID", value: "secret\u0085value"},
		{name: "tab value", key: "VALID", value: "secret\tvalue"},
		{name: "control value", key: "VALID", value: "secret\x01value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithEnv(map[string]string{tc.key: tc.value}),
				withRunner(f), withEngine(appleEngine{}))
			if err == nil {
				t.Fatal("invalid environment entry was accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid environment entry: %v", f.calls)
			}
		})
	}
}

func TestEnvValidationAllowsBackendCompatibleValues(t *testing.T) {
	env := map[string]string{
		"A":       "value with spaces",
		"A.B":     "value=with=equals",
		"EMPTY":   "",
		"UNICODE": "日本語",
	}
	if err := validateEnvMap(env); err != nil {
		t.Fatalf("valid environment entries rejected: %v", err)
	}
	if !envFileLocksSupported {
		return
	}
	isolateEnvFileRoot(t)
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(env),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("valid environment entries rejected: %v", err)
	}
}

func TestExecEnvValidationRejectsInvalidEntriesBeforeBackendCall(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "whitespace key", key: "BAD KEY", value: "value"},
		{name: "comment key", key: "#X", value: "1"},
		{name: "control value", key: "VALID", value: "secret\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil
			_, _, err := ctr.Exec(context.Background(), []string{"true"},
				WithExecEnv(map[string]string{tc.key: tc.value}))
			if err == nil {
				t.Fatal("invalid exec environment entry was accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid exec environment entry: %v", f.calls)
			}
		})
	}
}

func TestWriteEnvFileDoesNotTrustTMPDIR(t *testing.T) {
	isolateEnvFileRoot(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)

	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret=value"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})
	if got := envDirsIn(tmp); len(got) != 0 {
		t.Fatalf("environment file was created under untrusted TMPDIR %s: %v", tmp, got)
	}
}

func TestWriteEnvFileUsesPrivatePermissions(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret=value"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat env directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != envDirMode {
		t.Errorf("env directory mode = %04o, want %04o", got, envDirMode)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != envFileMode {
		t.Errorf("env file mode = %04o, want %04o", got, envFileMode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if got, want := string(data), "TOKEN=secret=value\n"; got != want {
		t.Errorf("env file = %q, want %q", got, want)
	}

	if err := cleanupEnvFile(dir); err != nil {
		t.Fatalf("cleanupEnvFile: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("env directory remains after cleanup: %v", err)
	}
}

func TestWriteEnvFileCleansUpAfterLateRootCloseError(t *testing.T) {
	base := t.TempDir()
	lateErr := errors.New("injected root lock close failure")
	path, dir, err := writeEnvFileAtWithRoot(base, map[string]string{"TOKEN": "secret"}, func(base string, fn func(string) error) error {
		return withEnvFileRootWithClose(base, fn, func(marker *os.File) error {
			_ = closeEnvFileLock(marker)
			return lateErr
		})
	})
	if !errors.Is(err, lateErr) {
		t.Fatalf("write error = %v, want late root close error", err)
	}
	if path != "" || dir != "" {
		t.Fatalf("write returned live path after cleanup: path=%q dir=%q", path, dir)
	}
	root := filepath.Join(base, envFileRootName)
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), envFileDirPrefix) ||
			strings.HasPrefix(entry.Name(), envFileStagingPrefix) ||
			strings.HasPrefix(entry.Name(), envFileTombstonePrefix) {
			t.Fatalf("environment artifact %q remained after root close failure", entry.Name())
		}
	}
}

func TestCleanupRetainsOwnershipAfterLateCloseError(t *testing.T) {
	root := isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	lateErr := errors.New("injected late lock close failure")
	err = cleanupEnvFileAtWithClose(filepath.Dir(root), dir, removeExpectedEnvChildren, func(lock *os.File) error {
		_ = closeEnvFileLock(lock)
		return lateErr
	})
	if !errors.Is(err, lateErr) {
		t.Fatalf("cleanup error = %v, want late close error", err)
	}
	state := loadEnvCleanupState(dir)
	if state == nil || !state.pending {
		t.Fatalf("cleanup ownership was lost after late close error: %#v", state)
	}
	if err := cleanupEnvFileAt(filepath.Dir(root), dir, removeExpectedEnvChildren); err != nil {
		t.Fatalf("cleanup retry after late close error: %v", err)
	}
	if state := loadEnvCleanupState(dir); state != nil {
		t.Fatalf("cleanup state remains after successful retry: %#v", state)
	}
}

func TestCleanupEnvFileReturnsRemovalFailureAndCanRetry(t *testing.T) {
	root := isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	removeFailure := errors.New("injected removal failure")
	attempts := 0
	err = cleanupEnvFileAt(filepath.Dir(root), dir, func(path string) error {
		attempts++
		if attempts == 1 {
			if err := os.Remove(filepath.Join(path, envFileDirMarkerName)); err != nil {
				return err
			}
			return removeFailure
		}
		return os.RemoveAll(path)
	})
	if !errors.Is(err, removeFailure) {
		t.Fatalf("cleanup error = %v, want injected removal failure", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("original environment path was not handed off to a tombstone: %v", err)
	}
	state := loadEnvCleanupState(dir)
	if state == nil || !state.pending || state.path == "" {
		t.Fatalf("cleanup ownership was not retained for retry: %#v", state)
	}
	if _, err := os.Stat(state.path); err != nil {
		t.Fatalf("cleanup tombstone is missing after failed cleanup: %v", err)
	}

	// cleanupEnvFileAt models the deferred retry used by Run, reuse, and
	// Exec. A partial first RemoveAll is allowed to finish on the retry.
	if err := cleanupEnvFileAt(filepath.Dir(root), dir, os.RemoveAll); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains after retry: %v", err)
	}
}

type envLifecycleRunner struct {
	*fakeRunner
	root string

	pullSawEnv bool
	runSawEnv  bool
	copySawEnv bool
	waitSawEnv bool
}

func (r *envLifecycleRunner) sawEnv() bool {
	return len(envDirsIn(r.root)) > 0
}

func (r *envLifecycleRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image", "pull":
		r.pullSawEnv = r.sawEnv()
	case "run":
		r.runSawEnv = r.sawEnv()
	case "cp":
		r.copySawEnv = r.sawEnv()
	}
	return r.fakeRunner.Run(ctx, args...)
}

type envLifecycleWait struct {
	runner *envLifecycleRunner
}

func (w envLifecycleWait) WaitUntilReady(context.Context, wait.Target) error {
	w.runner.waitSawEnv = w.runner.sawEnv()
	return nil
}

func TestRunEnvFileOnlyLivesForBackendRead(t *testing.T) {
	root := isolateEnvFileRoot(t)
	hostFile := filepath.Join(root, "host.txt")
	if err := os.WriteFile(hostFile, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &envLifecycleRunner{fakeRunner: newTestRunner(), root: root}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithEnv(map[string]string{"TOKEN": "secret"}),
		WithFiles(File{HostPath: hostFile, ContainerPath: "/tmp/host"}),
		WithWaitStrategy(envLifecycleWait{runner: runner}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.pullSawEnv {
		t.Error("env file existed during image pull")
	}
	if !runner.runSawEnv {
		t.Error("env file was not available while the backend read it")
	}
	if runner.copySawEnv {
		t.Error("env file remained during file copy")
	}
	if runner.waitSawEnv {
		t.Error("env file remained during readiness wait")
	}
	if got := envDirsIn(root); len(got) != 0 {
		t.Fatalf("env directories remain after Run: %v", got)
	}
}

type reuseEnvLifecycleRunner struct {
	*reuseCreateRunner
	root string

	pullSawEnv bool
	runSawEnv  bool
	copySawEnv bool
	waitSawEnv bool
}

func (r *reuseEnvLifecycleRunner) sawEnv() bool {
	return len(envDirsIn(r.root)) > 0
}

func (r *reuseEnvLifecycleRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image", "pull":
		r.pullSawEnv = r.sawEnv()
	case "run":
		r.runSawEnv = r.sawEnv()
	case "cp":
		r.copySawEnv = r.sawEnv()
	}
	return r.reuseCreateRunner.Run(ctx, args...)
}

type reuseEnvLifecycleWait struct {
	runner *reuseEnvLifecycleRunner
}

func (w reuseEnvLifecycleWait) WaitUntilReady(context.Context, wait.Target) error {
	w.runner.waitSawEnv = w.runner.sawEnv()
	return nil
}

func TestReuseEnvFileOnlyLivesForBackendRead(t *testing.T) {
	root := isolateEnvFileRoot(t)
	hostFile := filepath.Join(root, "host.txt")
	if err := os.WriteFile(hostFile, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &reuseEnvLifecycleRunner{
		reuseCreateRunner: newReuseCreateRunner(),
		root:              root,
	}
	runner.imagePresent = false
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("env-reuse"), WithReuse(), withRunner(runner), withEngine(appleEngine{}),
		WithEnv(map[string]string{"TOKEN": "secret"}),
		WithFiles(File{HostPath: hostFile, ContainerPath: "/tmp/host"}),
		WithWaitStrategy(reuseEnvLifecycleWait{runner: runner}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.pullSawEnv {
		t.Error("env file existed during reuse image pull")
	}
	if !runner.runSawEnv {
		t.Error("env file was not available while the backend read it")
	}
	if runner.copySawEnv {
		t.Error("env file remained during reuse file copy")
	}
	if runner.waitSawEnv {
		t.Error("env file remained during reuse readiness wait")
	}
	if got := envDirsIn(root); len(got) != 0 {
		t.Fatalf("env directories remain after reuse Run: %v", got)
	}
}

type envExecLifecycleRunner struct {
	*execRunner
	root   string
	sawEnv bool
}

func (r *envExecLifecycleRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		r.sawEnv = len(envDirsIn(r.root)) > 0
	}
	return r.execRunner.Run(ctx, args...)
}

func TestExecEnvFileRemovedAfterBackendRead(t *testing.T) {
	root := isolateEnvFileRoot(t)
	runner := &envExecLifecycleRunner{
		execRunner: &execRunner{fakeRunner: newTestRunner()},
		root:       root,
	}
	ctr := runTestContainer(t, runner)
	_, _, err := ctr.Exec(context.Background(), []string{"true"},
		WithExecEnv(map[string]string{"TOKEN": "secret"}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !runner.sawEnv {
		t.Error("env file was not available while the backend read it")
	}
	if got := envDirsIn(root); len(got) != 0 {
		t.Fatalf("env directories remain after Exec: %v", got)
	}
}

func TestCleanupStaleEnvFilesRespectsWriterLock(t *testing.T) {
	if !envFileLocksSupported {
		t.Skip("advisory env-file locks are unavailable on this platform")
	}
	root := isolateEnvFileRoot(t)
	dir := filepath.Join(root, envFileDirPrefix+"crashed")
	if err := os.Mkdir(dir, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker); err != nil {
		t.Fatal(err)
	}
	lock, err := openEnvFileNoFollow(filepath.Join(dir, envFileLockName), os.O_CREATE|os.O_RDWR, envFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := acquireEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, envFileName), []byte("SECRET"), envFileMode); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleEnvFiles(); err != nil {
		t.Fatalf("cleanupStaleEnvFiles: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live env directory was removed: %v", err)
	}
	if err := releaseEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()

	if err := cleanupStaleEnvFiles(); err != nil {
		t.Fatalf("cleanupStaleEnvFiles: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("unlocked env directory remains: %v", err)
	}
}

func isolateEnvFileRoot(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("LocalAppData", base)

	cache := base
	if runtime.GOOS == "darwin" {
		cache = filepath.Join(base, "Library", "Caches")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(cache, envFileRootName)
	if err := withEnvFileRoot(cache, func(string) error { return nil }); err != nil {
		if errors.Is(err, ErrEnvFileUnsupported) {
			t.Skip("secure environment files are unsupported on this platform")
		}
		t.Fatal(err)
	}
	return root
}

func envDirsIn(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), envFileDirPrefix) {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}
