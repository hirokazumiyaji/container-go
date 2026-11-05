package container

import (
	"context"
	"errors"
	"testing"
)

func TestOptionsRejectControlCharactersAndInvalidUTF8(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})

	tests := []struct {
		name  string
		apply func() error
	}{
		{"entrypoint CR", func() error { return WithEntrypoint("a\rb")(&config{}) }},
		{"entrypoint invalid UTF-8", func() error { return WithEntrypoint(invalidUTF8)(&config{}) }},
		{"working dir CR", func() error { return WithWorkingDir("/a\rb")(&config{}) }},
		{"working dir invalid UTF-8", func() error { return WithWorkingDir("/" + invalidUTF8)(&config{}) }},
		{"cmd NUL", func() error { return WithCmd("echo", "a\x00b")(&config{}) }},
		{"cmd invalid UTF-8", func() error { return WithCmd(invalidUTF8)(&config{}) }},
		{"exec working dir CR", func() error { return WithExecWorkDir("/a\rb")(&execConfig{}) }},
		{"exec working dir invalid UTF-8", func() error { return WithExecWorkDir("/" + invalidUTF8)(&execConfig{}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.apply()
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
		})
	}
}

func TestOptionsStillAcceptValidValues(t *testing.T) {
	if err := WithEntrypoint("/bin/sh")(&config{}); err != nil {
		t.Fatalf("WithEntrypoint rejected a valid entrypoint: %v", err)
	}
	if err := WithWorkingDir("/work")(&config{}); err != nil {
		t.Fatalf("WithWorkingDir rejected a valid directory: %v", err)
	}
	if err := WithCmd("echo", "hello world")(&config{}); err != nil {
		t.Fatalf("WithCmd rejected valid arguments: %v", err)
	}
	if err := WithExecWorkDir("/work")(&execConfig{}); err != nil {
		t.Fatalf("WithExecWorkDir rejected a valid directory: %v", err)
	}
}

func TestExecRejectsInvalidCommandArgumentsBeforeRunning(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	container := &Container{}

	for _, arg := range []string{"a\x00b", invalidUTF8} {
		_, _, _, err := container.execute(context.Background(), []string{"echo", arg}, nil, nil, false)
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("argument %q: error = %T %v, want *ValidationError", arg, err, err)
		}
	}
}
