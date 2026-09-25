//go:build integration

package container_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

// assertIntegrationCopyOutRejectsSpecialFiles exercises the Docker-only
// copy-out safety contract; Apple is tested separately for fail-closed behavior.
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
	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer setupCancel()
	for _, tc := range setup {
		code, out, err := ctr.Exec(setupCtx, []string{"sh", "-c", tc.cmd})
		if err != nil || code != 0 {
			var data []byte
			if out != nil {
				data, _ = io.ReadAll(out)
			}
			t.Fatalf("create %s: code=%d err=%v output=%q", tc.name, code, err, data)
		}
	}

	for _, tc := range setup {
		t.Run(tc.name, func(t *testing.T) {
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
				if !errors.Is(got.err, container.ErrCopyFileNotRegular) {
					t.Errorf("%s error = %v, want ErrCopyFileNotRegular", tc.name, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("CopyFileFromContainer blocked on %s", tc.name)
			}
		})
	}
}
