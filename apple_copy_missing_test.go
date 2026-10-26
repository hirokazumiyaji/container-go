package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Apple reports an absent container from cp inside a client-side
// "failed to copy into/from container" internalError whose cause is the
// server's notFound leaf. The matcher keeps the wrapper target-scoped and
// must not accept a local source-path notFound the CLI emits for a bad host
// path.
func TestAppleCopyMissingContainerForms(t *testing.T) {
	copyIn := []string{"cp", "/host/source", "myctr:/tmp/destination"}
	copyOut := []string{"cp", "myctr:/tmp/source", "/host/destination"}
	cases := []struct {
		name   string
		args   []string
		stderr string
		want   bool
	}{
		{
			name:   "copy in typed id leaf",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "notFound: \"container with ID myctr not found\"")`,
			want:   true,
		},
		{
			name:   "copy in unescaped typed id leaf",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "notFound: "container with ID myctr not found"")`,
			want:   true,
		},
		{
			name:   "copy in typed generic leaf",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "notFound: \"container not found: myctr\"")`,
			want:   true,
		},
		{
			name:   "copy out typed id leaf",
			args:   copyOut,
			stderr: `Error: internalError: "failed to copy from container myctr" (cause: "notFound: \"container with ID myctr not found\"")`,
			want:   true,
		},
		{
			name:   "flat copy in wrapper",
			args:   copyIn,
			stderr: `Error: failed to copy into container myctr: container with ID myctr not found`,
			want:   true,
		},
		{
			name:   "direct not found",
			args:   copyIn,
			stderr: `Error: notFound: "container not found: myctr"`,
			want:   true,
		},
		{
			name:   "wrong wrapper target",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container other" (cause: "notFound: \"container with ID other not found\"")`,
			want:   false,
		},
		{
			name:   "wrong cause target",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "notFound: \"container with ID other not found\"")`,
			want:   false,
		},
		{
			name:   "host source not found",
			args:   copyIn,
			stderr: `Error: notFound: "source path does not exist: /host/source"`,
			want:   false,
		},
		{
			name:   "container not running",
			args:   copyIn,
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "invalidState: \"container myctr is not running\"")`,
			want:   false,
		},
		{
			name:   "internal error without copy wrapper",
			args:   copyIn,
			stderr: `Error: internalError: "failed to get container" (cause: "notFound: \"container with ID myctr not found\"")`,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &cli.CLIError{
				Binary:   "container",
				Args:     tc.args,
				ExitCode: 1,
				Stderr:   tc.stderr,
			}
			if got := (appleEngine{}).containerMissing(err); got != tc.want {
				t.Fatalf("containerMissing(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
			if got := errors.Is(wrapNotFoundFor(appleEngine{}, err), ErrContainerNotFound); got != tc.want {
				t.Fatalf("wrapNotFoundFor(%q) ErrContainerNotFound = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestCopyToContainerAppleMissingContainer exercises the name-addressed cp
// path end to end: CopyToContainer must match the Apple CLI's notFound
// classification rather than returning only the raw CLIError.
func TestCopyToContainerAppleMissingContainer(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "typed copy in envelope",
			stderr: `Error: internalError: "failed to copy into container myctr" (cause: "notFound: \"container with ID myctr not found\"")`,
			want:   true,
		},
		{
			name:   "direct not found",
			stderr: `Error: notFound: "container not found: myctr"`,
			want:   true,
		},
		{
			name:   "host source not found",
			stderr: `Error: notFound: "source path does not exist: /host/source"`,
			want:   false,
		},
		{
			name:   "wrong container",
			stderr: `Error: internalError: "failed to copy into container other" (cause: "notFound: \"container with ID other not found\"")`,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.failPrefix = "cp"
			f.failStderr = tc.stderr

			source := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := ctr.CopyToContainer(context.Background(), source, "/tmp/destination")
			if err == nil {
				t.Fatal("CopyToContainer succeeded, want error")
			}
			if got := errors.Is(err, ErrContainerNotFound); got != tc.want {
				t.Fatalf("CopyToContainer error = %v, want ErrContainerNotFound=%v", err, tc.want)
			}
		})
	}
}
