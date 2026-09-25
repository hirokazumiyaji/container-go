//go:build integration

package container_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

// assertIntegrationCopyOutRejectsSpecialFiles exercises the Docker-only
// copy-out safety contract; Apple is tested separately for fail-closed behavior.
// Windows may reject or skip materializing Unix links and FIFOs on the host,
// so those cases are skipped when the host cannot complete the copy safely.
func assertIntegrationCopyOutRejectsSpecialFiles(t *testing.T, ctx context.Context, ctr *container.Container) {
	t.Helper()

	setup := []struct {
		name string
		cmd  string
		path string
	}{
		{name: "symlink", cmd: "ln -s /tmp/hello.txt /tmp/containergo-copy-link", path: "/tmp/containergo-copy-link"},
		{name: "fifo", cmd: "mkfifo /tmp/containergo-copy-fifo", path: "/tmp/containergo-copy-fifo"},
		{name: "directory", cmd: "mkdir /tmp/containergo-copy-dir", path: "/tmp/containergo-copy-dir"},
	}

	for _, tc := range setup {
		t.Run(tc.name, func(t *testing.T) {
			setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
			code, out, err := ctr.Exec(setupCtx, []string{"sh", "-c", tc.cmd})
			if err != nil || code != 0 {
				var data []byte
				if out != nil {
					data, _ = io.ReadAll(out)
				}
				setupCancel()
				if runtime.GOOS == "windows" {
					t.Skipf("Windows host rejected special-file setup for %s: code=%d err=%v output=%q", tc.name, code, err, data)
				}
				t.Fatalf("create %s: code=%d err=%v output=%q", tc.name, code, err, data)
			}
			setupCancel()

			copyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			result := make(chan struct {
				rc  io.ReadCloser
				err error
			}, 1)
			go func() {
				rc, err := ctr.CopyFileFromContainer(copyCtx, tc.path)
				result <- struct {
					rc  io.ReadCloser
					err error
				}{rc: rc, err: err}
			}()

			select {
			case got := <-result:
				if got.rc != nil {
					_ = got.rc.Close()
				}
				if got.err == nil {
					t.Fatalf("CopyFileFromContainer accepted %s", tc.name)
				}
				if errors.Is(got.err, container.ErrCopyFileNotRegular) {
					return
				}
				if runtime.GOOS == "windows" {
					// Windows Go 1.23-1.25 intentionally fail closed before
					// invoking docker cp; newer hosts may also reject a special
					// file while materializing it.
					t.Skipf("Windows host cannot represent or inspect %s: %v", tc.name, got.err)
				}
				t.Errorf("%s error = %v, want ErrCopyFileNotRegular", tc.name, got.err)
			case <-time.After(5 * time.Second):
				t.Fatalf("CopyFileFromContainer blocked on %s", tc.name)
			}
		})
	}
}
