package container

import (
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

func TestPublicErrorRedactionSurvivesErrorsAs(t *testing.T) {
	const secret = "public-error-secret-265"
	wrap := func(err error) error { return safePublicError(err) }

	t.Run("ConfigError", func(t *testing.T) {
		original := &ConfigError{Backend: "docker", Detail: secret}
		var got *ConfigError
		if !errors.As(wrap(original), &got) {
			t.Fatal("errors.As did not find *ConfigError")
		}
		if got == original || strings.Contains(got.Detail, secret) {
			t.Fatalf("ConfigError = %#v, want a redacted clone", got)
		}
	})

	t.Run("ValidationError", func(t *testing.T) {
		original := &ValidationError{Option: secret, Field: secret, Value: secret, Message: secret, Err: errors.New(secret)}
		var got *ValidationError
		if !errors.As(wrap(original), &got) {
			t.Fatal("errors.As did not find *ValidationError")
		}
		value, _ := got.Value.(string)
		if got == original || strings.Contains(got.Option, secret) || strings.Contains(got.Field, secret) ||
			strings.Contains(value, secret) || strings.Contains(got.Message, secret) ||
			(got.Err != nil && strings.Contains(got.Err.Error(), secret)) {
			t.Fatalf("ValidationError = %#v, want all fields redacted", got)
		}
	})

	t.Run("ExecTerminationError", func(t *testing.T) {
		original := &ExecTerminationError{Err: errors.New(secret)}
		var got *ExecTerminationError
		if !errors.As(wrap(original), &got) {
			t.Fatal("errors.As did not find *ExecTerminationError")
		}
		if got == original || strings.Contains(got.Err.Error(), secret) {
			t.Fatalf("ExecTerminationError = %#v, want a redacted clone", got)
		}
	})

	t.Run("CleanupError", func(t *testing.T) {
		original := &CleanupError{Container: secret, Err: errors.New(secret)}
		var got *CleanupError
		if !errors.As(wrap(original), &got) {
			t.Fatal("errors.As did not find *CleanupError")
		}
		if got == original || strings.Contains(got.Container, secret) || strings.Contains(got.Err.Error(), secret) {
			t.Fatalf("CleanupError = %#v, want both fields redacted", got)
		}
	})

	t.Run("wait.ConfigError", func(t *testing.T) {
		original := &wait.ConfigError{Strategy: secret, Field: secret, Value: secret, Reason: secret}
		var got *wait.ConfigError
		if !errors.As(wrap(original), &got) {
			t.Fatal("errors.As did not find *wait.ConfigError")
		}
		if got == original || strings.Contains(got.Strategy, secret) || strings.Contains(got.Field, secret) ||
			strings.Contains(got.Value, secret) || strings.Contains(got.Reason, secret) {
			t.Fatalf("wait.ConfigError = %#v, want all fields redacted", got)
		}
	})
}
