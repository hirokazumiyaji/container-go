package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func assertPublicValidationError(t *testing.T, err error, option, field string, value any, message string) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid input was accepted")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if validationErr.Option != option {
		t.Fatalf("validation option = %q, want %q", validationErr.Option, option)
	}
	if validationErr.Field != field {
		t.Fatalf("validation field = %q, want %q", validationErr.Field, field)
	}
	if !reflect.DeepEqual(validationErr.Value, value) {
		t.Fatalf("validation value = %#v, want %#v", validationErr.Value, value)
	}
	if err.Error() != message {
		t.Fatalf("error = %q, want %q", err, message)
	}
}

func assertRedactedMountValidationError(t *testing.T, err error, message string) {
	t.Helper()
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Option != "WithMounts" || validationErr.Field != "mount" {
		t.Fatalf("error = %v, want WithMounts mount validation", err)
	}
	if got, ok := validationErr.Value.(Mount); !ok || got.Source != "[REDACTED]" || got.Target != "[REDACTED]" {
		t.Fatalf("validation mount = %#v, want redacted source and target", validationErr.Value)
	}
	if err.Error() != message {
		t.Fatalf("error = %q, want %q", err, message)
	}
}

func TestCopyMethodsRejectInvalidContainerPathAsValidationError(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		option string
		call   func(*Container, string) error
	}{
		{
			name:   "copy to",
			path:   "relative/path",
			option: "CopyToContainer",
			call: func(c *Container, path string) error {
				return c.CopyToContainer(context.Background(), t.TempDir(), path)
			},
		},
		{
			name:   "copy from",
			path:   "relative/path",
			option: "CopyFileFromContainer",
			call: func(c *Container, path string) error {
				rc, err := c.CopyFileFromContainer(context.Background(), path)
				if rc != nil {
					_ = rc.Close()
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			err := tt.call(ctr, tt.path)
			assertPublicValidationError(t, err, tt.option, "containerPath", tt.path,
				fmt.Sprintf("container path %q must be absolute", tt.path))
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid path: %v", f.calls)
			}
		})
	}
}

func TestCopyFileFromContainerRejectsRootAndTrailingSlashAsValidationError(t *testing.T) {
	for _, path := range []string{"/", "/foo/", "/foo/bar/"} {
		t.Run(path, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			rc, err := ctr.CopyFileFromContainer(context.Background(), path)
			if rc != nil {
				_ = rc.Close()
			}
			want := fmt.Sprintf("copy file from container %q: cannot copy directory or root as a single file", path)
			assertPublicValidationError(t, err, "CopyFileFromContainer", "containerPath", path, want)
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid copy path: %v", f.calls)
			}
		})
	}
}

func TestCopyToContainerFilesystemErrorIsNotValidationError(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	err := ctr.CopyToContainer(context.Background(), filepath.Join(t.TempDir(), "missing"), "/x")
	if err == nil {
		t.Fatal("missing host path was accepted")
	}
	if errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want filesystem error without validation wrapping", err)
	}
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		t.Fatalf("error = %T, want no *ValidationError", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestCopyFileFromContainerFilesystemErrorIsNotValidationError(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	f := &cpRunner{
		fakeRunner:  newTestRunner(),
		materialize: func(string) error { return nil },
	}
	ctr := runCopyDockerTestContainer(t, f)
	f.calls = nil

	_, err := ctr.CopyFileFromContainer(context.Background(), "/missing")
	if err == nil {
		t.Fatal("missing materialized file was accepted")
	}
	if errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want filesystem error without validation wrapping", err)
	}
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		t.Fatalf("error = %T, want no *ValidationError", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestMappedPortAndEndpointRejectInvalidPortAsValidationError(t *testing.T) {
	const port = "not-a-port"
	tests := []struct {
		name   string
		option string
		call   func(*Container) error
	}{
		{
			name:   "mapped port",
			option: "MappedPort",
			call: func(c *Container) error {
				_, err := c.MappedPort(context.Background(), port)
				return err
			},
		},
		{
			name:   "endpoint",
			option: "Endpoint",
			call: func(c *Container) error {
				_, err := c.Endpoint(context.Background(), port)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			err := tt.call(ctr)
			value := any(port)
			message := fmt.Sprintf("invalid port %q: port must be 1-65535", port)
			if tt.option == "Endpoint" {
				value = "[REDACTED]"
				message = `invalid port "[REDACTED]": port must be 1-65535`
			}
			assertPublicValidationError(t, err, tt.option, "port", value, message)
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid port: %v", f.calls)
			}
		})
	}
}

func TestMappedPortAndEndpointLeaveUndeclaredPortErrorUnwrapped(t *testing.T) {
	const port = "5432/tcp"
	tests := []struct {
		name string
		call func(*Container) error
	}{
		{
			name: "mapped port",
			call: func(c *Container) error {
				_, err := c.MappedPort(context.Background(), port)
				return err
			},
		},
		{
			name: "endpoint",
			call: func(c *Container) error {
				_, err := c.Endpoint(context.Background(), port)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			err := tt.call(ctr)
			if !errors.Is(err, ErrPortNotExposed) {
				t.Fatalf("error = %v, want ErrPortNotExposed", err)
			}
			if errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want no validation wrapping", err)
			}
			var validationErr *ValidationError
			if errors.As(err, &validationErr) {
				t.Fatalf("error = %T, want no *ValidationError", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called for undeclared port: %v", f.calls)
			}
		})
	}
}

func TestMountValidationPreservesLegacyDelimiterErrors(t *testing.T) {
	for _, bad := range []string{",", "=", "\x00"} {
		t.Run(fmt.Sprintf("source-%q", bad), func(t *testing.T) {
			mount := Mount{Type: MountBind, Source: "/host" + bad + "data", Target: "/data"}
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithMounts(mount), withRunner(f), withEngine(appleEngine{}))
			want := `mount "[REDACTED]" -> "[REDACTED]": paths must not contain ',' or '='`
			assertRedactedMountValidationError(t, err, want)
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid mount: %v", f.calls)
			}
		})
	}
}

func TestMountValidationRejectsCRLFSeparately(t *testing.T) {
	for _, bad := range []string{"\r", "\n"} {
		t.Run(fmt.Sprintf("source-%q", bad), func(t *testing.T) {
			mount := Mount{Type: MountBind, Source: "/host" + bad + "data", Target: "/data"}
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithMounts(mount), withRunner(f), withEngine(appleEngine{}))
			want := `mount "[REDACTED]" -> "[REDACTED]": paths must not contain control characters`
			assertRedactedMountValidationError(t, err, want)
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid mount: %v", f.calls)
			}
		})
	}
}
