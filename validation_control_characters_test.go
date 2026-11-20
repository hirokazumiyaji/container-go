package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func assertCommandArgumentValidationError(t *testing.T, err error, option string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted an invalid command argument", option)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if validationErr.Option != option || validationErr.Field != "arg" {
		t.Fatalf("validation option/field = %q/%q, want %s/arg", validationErr.Option, validationErr.Field, option)
	}
	if validationErr.Value != nil {
		t.Fatalf("validation value = %#v, want nil", validationErr.Value)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %q, leaks a rejected argument", err)
	}
}

func TestRunRejectsControlCharactersAndInvalidUTF8InWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"carriage return", "/data\r"},
		{"newline", "/data\n"},
		{"nul", "/data\x00"},
		{"invalid UTF-8", "/data\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithWorkingDir(tc.dir), withRunner(f), withEngine(appleEngine{}))
			assertOptionValidationError(t, err, "WithWorkingDir")
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid working directory: %v", f.calls)
			}
		})
	}
}

func TestRunRejectsControlCharactersAndInvalidUTF8InEntrypoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entrypoint string
	}{
		{"carriage return", "/entry.sh\r"},
		{"newline", "/entry.sh\n"},
		{"nul", "/entry.sh\x00"},
		{"invalid UTF-8", "/entry.sh\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithEntrypoint(tc.entrypoint), withRunner(f), withEngine(appleEngine{}))
			assertOptionValidationError(t, err, "WithEntrypoint")
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid entrypoint: %v", f.calls)
			}
		})
	}
}

func TestExecWorkDirRejectsControlCharactersAndInvalidUTF8(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"carriage return", "/data\r"},
		{"newline", "/data\n"},
		{"nul", "/data\x00"},
		{"invalid UTF-8", "/data\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecWorkDir(tc.dir))
			assertOptionValidationError(t, err, "WithExecWorkDir")
		})
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called despite invalid exec working directory: %v", f.calls)
	}
}

func TestWithCmdRejectsInvalidCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		arg  string
	}{
		{"nul", "secret\x00value"},
		{"invalid UTF-8", "secret\xffvalue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithCmd("echo", tc.arg), withRunner(f), withEngine(appleEngine{}))
			assertCommandArgumentValidationError(t, err, "WithCmd")
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid command argument: %v", f.calls)
			}
		})
	}
}

func TestWithCmdAcceptsEmptyAndControlFreeArguments(t *testing.T) {
	f := newTestRunner()
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithCmd("echo", ""), withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run rejected valid command arguments: %v", err)
	}
}

func TestExecRejectsInvalidCommandArguments(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	f.calls = nil

	for _, tc := range []struct {
		name string
		arg  string
	}{
		{"nul", "secret\x00value"},
		{"invalid UTF-8", "secret\xffvalue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, output, err := ctr.Exec(context.Background(), []string{"echo", tc.arg})
			if code != 0 || output != nil {
				t.Fatalf("Exec result = (%d, %v), want zero values", code, output)
			}
			assertCommandArgumentValidationError(t, err, "Exec")
		})
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called despite invalid exec arguments: %v", f.calls)
	}
}

func TestExecAcceptsValidArguments(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "ok"}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"echo", ""}); err != nil {
		t.Fatalf("Exec rejected valid command arguments: %v", err)
	}
}
