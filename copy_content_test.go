package container

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// contentCaptureRunner records what the CLI actually received during cp. It
// reads the staged file while the backend call is in flight, which proves the
// copy path materializes content before invoking the backend.
type contentCaptureRunner struct {
	*fakeRunner
	mu         sync.Mutex
	stagedPath string
	stagedData []byte
	stagedMode fs.FileMode
}

func (r *contentCaptureRunner) captureStaged(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.stagedPath = path
	r.stagedData = data
	r.stagedMode = info.Mode().Perm()
	r.mu.Unlock()
	return nil
}

func (r *contentCaptureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "cp" && strings.Contains(args[2], ":") {
		if err := r.captureStaged(args[1]); err != nil {
			return nil, nil, err
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *contentCaptureRunner) staged() (string, []byte, fs.FileMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stagedPath, append([]byte(nil), r.stagedData...), r.stagedMode
}

func assertCopyStagingRemoved(t *testing.T, staged string) {
	t.Helper()
	if staged == "" {
		t.Fatal("no staged source was captured during cp")
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staged file %q remains after copy: %v", staged, err)
	}
	dir := filepath.Dir(staged)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging directory %q remains after copy: %v", dir, err)
	}
}

func assertContentAbsent(t *testing.T, err error, content string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		if strings.Contains(rendered, content) {
			t.Fatalf("error contains copy content: %q", rendered)
		}
	}
}

func TestWithFilesCopiesContentAndCleansStaging(t *testing.T) {
	root := copyStagingTestRoot(t)
	before := copyStagingEntryCount(t, root)
	const payload = "CREATE TABLE tokens (value text);"

	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runTestContainer(t, runner, WithFiles(File{Content: []byte(payload), ContainerPath: "/init/schema.sql"}))

	call := runner.callWith("cp")
	if len(call) != 3 || call[0] != "cp" || call[2] != "myctr:/init/schema.sql" {
		t.Fatalf("cp args = %v", call)
	}
	for _, arg := range call {
		if strings.Contains(arg, payload) {
			t.Fatalf("copy content appeared in argv: %q", arg)
		}
	}

	staged, data, mode := runner.staged()
	if string(data) != payload {
		t.Fatalf("staged content = %q, want %q", data, payload)
	}
	if runtime.GOOS != "windows" && mode != 0o600 {
		t.Errorf("staged content mode = %04o, want 0600", mode)
	}
	assertCopyStagingRemoved(t, staged)
	if after := copyStagingEntryCount(t, root); after != before {
		t.Fatalf("staging entries after copy = %d, want %d", after, before)
	}
}

func TestWithFilesContentModeAppliesToStagedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	copyStagingTestRoot(t)
	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runTestContainer(t, runner,
		WithFiles(File{Content: []byte("secret"), ContainerPath: "/etc/key", Mode: 0o640}))

	_, _, mode := runner.staged()
	if mode != 0o640 {
		t.Fatalf("staged content mode = %04o, want 0640", mode)
	}
}

func TestWithFilesAllowsEmptyContent(t *testing.T) {
	copyStagingTestRoot(t)
	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runTestContainer(t, runner, WithFiles(File{Content: []byte{}, ContainerPath: "/empty"}))

	staged, data, _ := runner.staged()
	if len(data) != 0 {
		t.Fatalf("staged content = %q, want empty", data)
	}
	assertCopyStagingRemoved(t, staged)
}

func TestWithFilesHostPathModeOverridesStagedCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	copyStagingTestRoot(t)
	source := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(source, []byte("key=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runTestContainer(t, runner,
		WithFiles(File{HostPath: source, ContainerPath: "/etc/config.txt", Mode: 0o600}))

	staged, data, mode := runner.staged()
	if string(data) != "key=value" {
		t.Fatalf("staged content = %q", data)
	}
	if mode != 0o600 {
		t.Fatalf("staged mode = %04o, want 0600", mode)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("caller file mode = %04o, want unchanged 0644", info.Mode().Perm())
	}
	assertCopyStagingRemoved(t, staged)
}

func TestWithFilesHostPathPreservesModeWhenModeIsZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	copyStagingTestRoot(t)
	source := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(source, []byte("key=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o640); err != nil {
		t.Fatal(err)
	}

	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runTestContainer(t, runner,
		WithFiles(File{HostPath: source, ContainerPath: "/etc/config.txt"}))

	_, _, mode := runner.staged()
	if mode != 0o640 {
		t.Fatalf("staged mode = %04o, want source mode 0640", mode)
	}
}

func TestWithFilesRejectsHostPathAndContentTogether(t *testing.T) {
	const secret = "mutually-exclusive-content-263"
	source := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(source, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := newCountingRunner(newTestRunner())
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{HostPath: source, Content: []byte(secret), ContainerPath: "/etc/config"}))
	if err == nil {
		t.Fatal("Run accepted both hostPath and content")
	}
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Option != "WithFiles" || validation.Field != "content" {
		t.Fatalf("error = %v, want WithFiles/content ValidationError", err)
	}
	if validation.Value != nil {
		t.Fatalf("validation value = %#v, want nil for content", validation.Value)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	assertContentAbsent(t, err, secret)
	if got := runner.count(); got != 0 {
		t.Fatalf("backend was called %d times for rejected options", got)
	}
}

func TestWithFilesRejectsMissingSource(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{ContainerPath: "/etc/config"}))
	assertPublicValidationError(t, err, "WithFiles", "hostPath", "", "host path must not be empty")
	if got := runner.count(); got != 0 {
		t.Fatalf("backend was called %d times for rejected options", got)
	}
}

func TestWithFilesRejectsNonPermissionMode(t *testing.T) {
	const secret = "invalid-mode-content-263"
	cases := []struct {
		name string
		mode fs.FileMode
	}{
		{"setuid", fs.ModeSetuid | 0o755},
		{"setgid", fs.ModeSetgid | 0o755},
		{"sticky", fs.ModeSticky | 0o755},
		{"directory", fs.ModeDir | 0o755},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newCountingRunner(newTestRunner())
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
				WithFiles(File{Content: []byte(secret), ContainerPath: "/etc/key", Mode: tc.mode}))
			if err == nil {
				t.Fatal("Run accepted a non-permission mode")
			}
			var validation *ValidationError
			if !errors.As(err, &validation) || validation.Option != "WithFiles" || validation.Field != "mode" {
				t.Fatalf("error = %v, want WithFiles/mode ValidationError", err)
			}
			if validation.Value != tc.mode {
				t.Fatalf("validation value = %#v, want %#v", validation.Value, tc.mode)
			}
			assertContentAbsent(t, err, secret)
			if got := runner.count(); got != 0 {
				t.Fatalf("backend was called %d times for rejected options", got)
			}
		})
	}
}

func TestWithFilesRejectsOversizedContent(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithFiles(File{Content: make([]byte, MaxCopyToContainerSize+1), ContainerPath: "/large"}))
	if err == nil {
		t.Fatal("Run accepted content beyond MaxCopyToContainerSize")
	}
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Option != "WithFiles" || validation.Field != "content" {
		t.Fatalf("error = %v, want WithFiles/content ValidationError", err)
	}
	if !errors.Is(err, ErrCopySourceTooLarge) || !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrCopySourceTooLarge and ErrInvalidOption", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("backend was called %d times for rejected options", got)
	}
}

func TestCopyContentToContainerCopiesAndCleansStaging(t *testing.T) {
	root := copyStagingTestRoot(t)
	before := copyStagingEntryCount(t, root)
	const payload = "PASSWORD=content-secret-263\n"

	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	if err := ctr.CopyContentToContainer(context.Background(), []byte(payload), "/etc/app.env", 0o640); err != nil {
		t.Fatalf("CopyContentToContainer: %v", err)
	}
	call := runner.callWith("cp")
	if len(call) != 3 || call[2] != "myctr:/etc/app.env" {
		t.Fatalf("cp args = %v", call)
	}
	for _, arg := range call {
		if strings.Contains(arg, payload) {
			t.Fatalf("copy content appeared in argv: %q", arg)
		}
	}
	staged, data, mode := runner.staged()
	if string(data) != payload {
		t.Fatalf("staged content = %q, want %q", data, payload)
	}
	if runtime.GOOS != "windows" && mode != 0o640 {
		t.Errorf("staged content mode = %04o, want 0640", mode)
	}
	assertCopyStagingRemoved(t, staged)
	if after := copyStagingEntryCount(t, root); after != before {
		t.Fatalf("staging entries after copy = %d, want %d", after, before)
	}
}

func TestCopyContentToContainerDefaultsTo0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	copyStagingTestRoot(t)
	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	if err := ctr.CopyContentToContainer(context.Background(), []byte("secret"), "/etc/key", 0); err != nil {
		t.Fatalf("CopyContentToContainer: %v", err)
	}
	_, _, mode := runner.staged()
	if mode != 0o600 {
		t.Fatalf("staged content mode = %04o, want 0600", mode)
	}
}

func TestCopyContentToContainerAllowsEmptyContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
	}{
		{"nil", nil},
		{"empty slice", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyStagingTestRoot(t)
			runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
			ctr := runTestContainer(t, runner)
			if err := ctr.CopyContentToContainer(context.Background(), tc.content, "/empty", 0); err != nil {
				t.Fatalf("CopyContentToContainer: %v", err)
			}
			staged, data, _ := runner.staged()
			if len(data) != 0 {
				t.Fatalf("staged content = %q, want empty", data)
			}
			assertCopyStagingRemoved(t, staged)
		})
	}
}

func TestCopyContentToContainerRejectsNonPermissionMode(t *testing.T) {
	const secret = "direct-invalid-mode-content-263"
	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)
	callsBefore := len(runner.calls)

	err := ctr.CopyContentToContainer(context.Background(), []byte(secret), "/etc/key", fs.ModeSetuid|0o755)
	if err == nil {
		t.Fatal("CopyContentToContainer accepted a non-permission mode")
	}
	var validation *ValidationError
	if !errors.As(err, &validation) ||
		validation.Option != "CopyContentToContainer" ||
		validation.Field != "mode" {
		t.Fatalf("error = %v, want CopyContentToContainer/mode ValidationError", err)
	}
	assertContentAbsent(t, err, secret)
	if got := len(runner.calls); got != callsBefore {
		t.Fatalf("backend was called for a rejected mode: %d -> %d", callsBefore, got)
	}
}

func TestCopyContentToContainerCleansStagingAfterFailure(t *testing.T) {
	copyStagingTestRoot(t)
	runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
	runner.failPrefix = "cp"
	ctr := runTestContainer(t, runner)

	err := ctr.CopyContentToContainer(context.Background(), []byte("secret"), "/etc/key", 0)
	if err == nil {
		t.Fatal("copy unexpectedly succeeded")
	}
	staged, _, _ := runner.staged()
	assertCopyStagingRemoved(t, staged)
}

type cancelingContentRunner struct {
	*contentCaptureRunner
	cancel context.CancelFunc
}

func (r *cancelingContentRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 3 && args[0] == "cp" && strings.Contains(args[2], ":") {
		if err := r.captureStaged(args[1]); err != nil {
			return nil, nil, err
		}
		r.cancel()
		return nil, nil, ctx.Err()
	}
	return r.contentCaptureRunner.Run(ctx, args...)
}

func TestCopyContentToContainerCleansStagingAfterCancellation(t *testing.T) {
	copyStagingTestRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &cancelingContentRunner{
		contentCaptureRunner: &contentCaptureRunner{fakeRunner: newTestRunner()},
		cancel:               cancel,
	}
	ctr := runTestContainer(t, runner)

	err := ctr.CopyContentToContainer(ctx, []byte("secret"), "/etc/key", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	staged, _, _ := runner.staged()
	assertCopyStagingRemoved(t, staged)
}

func TestCopyContentErrorsNeverExposeContent(t *testing.T) {
	const secret = "diagnostic-content-secret-263"

	t.Run("with files copy failure", func(t *testing.T) {
		copyStagingTestRoot(t)
		runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
		runner.failPrefix = "cp"
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
			WithFiles(File{Content: []byte(secret), ContainerPath: "/etc/key"}))
		assertContentAbsent(t, err, secret)
		if !strings.Contains(err.Error(), "content") {
			t.Fatalf("error = %q, want the content source label", err)
		}
		if staged, _, _ := runner.staged(); staged == "" {
			t.Fatal("copy failure did not reach the staged source")
		} else {
			assertCopyStagingRemoved(t, staged)
		}
	})

	t.Run("copy content failure", func(t *testing.T) {
		copyStagingTestRoot(t)
		runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
		runner.failPrefix = "cp"
		ctr := runTestContainer(t, runner)
		err := ctr.CopyContentToContainer(context.Background(), []byte(secret), "/etc/key", 0)
		assertContentAbsent(t, err, secret)
	})

	t.Run("container path validation", func(t *testing.T) {
		runner := &contentCaptureRunner{fakeRunner: newTestRunner()}
		ctr := runTestContainer(t, runner)
		err := ctr.CopyContentToContainer(context.Background(), []byte(secret), "relative", 0)
		assertContentAbsent(t, err, secret)
	})
}
