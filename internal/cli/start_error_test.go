package cli

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

func TestPermanentStartErrorClassification(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"missing": {
			err:  &os.PathError{Op: "fork/exec", Path: "backend", Err: syscall.ENOENT},
			want: true,
		},
		"permission": {
			err:  &os.PathError{Op: "fork/exec", Path: "backend", Err: syscall.EACCES},
			want: true,
		},
		"not directory": {
			err:  &os.PathError{Op: "fork/exec", Path: "backend", Err: syscall.ENOTDIR},
			want: true,
		},
		"executable format": {
			err:  &os.PathError{Op: "fork/exec", Path: "backend", Err: syscall.ENOEXEC},
			want: true,
		},
		"lookup": {
			err:  &exec.Error{Name: "backend", Err: syscall.ENOENT},
			want: true,
		},
		"temporary resource": {
			err:  &os.PathError{Op: "fork/exec", Path: "backend", Err: syscall.EAGAIN},
			want: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := PermanentStartError(tc.err); got != tc.want {
				t.Fatalf("PermanentStartError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestPermanentStartErrorClassifiesWindowsBadExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only executable-format error")
	}
	err := &os.PathError{Op: "fork/exec", Path: "backend.exe", Err: syscall.Errno(193)}
	if !PermanentStartError(err) {
		t.Fatalf("PermanentStartError(%v) = false, want true", err)
	}
}
