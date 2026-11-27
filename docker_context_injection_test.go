package container

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
)

func TestDockerContextOptionsRejectFlagShapedNames(t *testing.T) {
	invalid := []string{
		`--format=[{"Name":"x","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`,
		"-x",
		"--",
		"ctx\n",
		"ctx ctx",
		"ctx=1",
		"ctx/1",
		"ctx\x00",
		"-",
	}
	options := []struct {
		option string
		apply  func(string) error
	}{
		{"WithDockerContext", func(name string) error { return WithDockerContext(name)(newConfig()) }},
		{"WithClientDockerContext", func(name string) error { return WithClientDockerContext(name)(&clientConfig{}) }},
		{"WithPullDockerContext", func(name string) error { return WithPullDockerContext(name)(&pullConfig{}) }},
	}
	for _, opt := range options {
		for i, name := range invalid {
			t.Run(opt.option+"/"+strconv.Itoa(i), func(t *testing.T) {
				err := opt.apply(name)
				if !errors.Is(err, ErrInvalidOption) {
					t.Fatalf("%s(%q) error = %v, want ErrInvalidOption", opt.option, name, err)
				}
				var validationErr *ValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("%s(%q) error = %T, want *ValidationError", opt.option, name, err)
				}
				if validationErr.Option != opt.option {
					t.Errorf("validation option = %q, want %q", validationErr.Option, opt.option)
				}
			})
		}
	}
}

func TestDockerContextOptionsAcceptTheContextGrammar(t *testing.T) {
	valid := []string{"", "default", "my-ctx_1.2", "A1"}
	for _, name := range valid {
		if err := WithDockerContext(name)(newConfig()); err != nil {
			t.Errorf("WithDockerContext(%q) = %v, want nil", name, err)
		}
		if err := WithClientDockerContext(name)(&clientConfig{}); err != nil {
			t.Errorf("WithClientDockerContext(%q) = %v, want nil", name, err)
		}
		if err := WithPullDockerContext(name)(&pullConfig{}); err != nil {
			t.Errorf("WithPullDockerContext(%q) = %v, want nil", name, err)
		}
	}
}

func TestDockerContextInspectUsesSeparatorAndChecksName(t *testing.T) {
	ctx := context.Background()

	t.Run("named context", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"ssh-ctx","Endpoints":{"docker":{"Host":"ssh://user@remote-box:22"}}}]`

		name, host, err := resolveDockerContextEndpoint(ctx, f, "ssh-ctx")
		if err != nil {
			t.Fatalf("resolveDockerContextEndpoint: %v", err)
		}
		if name != "ssh-ctx" || host != "ssh://user@remote-box:22" {
			t.Errorf("resolved name/host = %q/%q, want ssh-ctx/ssh://user@remote-box:22", name, host)
		}
		if call := f.callWith("context"); !slices.Equal(call, []string{"context", "inspect", "--", "ssh-ctx"}) {
			t.Errorf("context inspect args = %v, want a -- separator before the name", call)
		}
	})

	t.Run("mismatched reported name", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"local","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`

		_, _, err := resolveDockerContextEndpoint(ctx, f, "remote")
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) {
			t.Fatalf("error = %T, want *ConfigError", err)
		}
	})

	t.Run("flag-shaped name rejected before the CLI", func(t *testing.T) {
		f := newDockerTestRunner()

		_, _, err := resolveDockerContextEndpoint(ctx, f, "--format=[]")
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
		if call := f.callWith("context"); call != nil {
			t.Fatalf("context inspect ran with an unvalidated name: %v", call)
		}
	})
}

func TestDockerAmbientContextRejectsFlagShapedName(t *testing.T) {
	const injected = `--format=[{"Name":"x","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`

	for name, setup := range map[string]func() *fakeRunner{
		"process environment": func() *fakeRunner {
			t.Setenv("DOCKER_HOST", "")
			t.Setenv("DOCKER_CONTEXT", injected)
			return newDockerTestRunner()
		},
		"runner environment": func() *fakeRunner {
			t.Setenv("DOCKER_HOST", "")
			t.Setenv("DOCKER_CONTEXT", "")
			f := newDockerTestRunner()
			f.env = []string{"DOCKER_CONTEXT=" + injected}
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup()
			_, err := (dockerEngine{}).resolveTarget(context.Background(), f)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("resolveTarget error = %v, want ErrInvalidConfig", err)
			}
			if call := f.callWith("context"); call != nil {
				t.Fatalf("context inspect ran with an unvalidated name: %v", call)
			}
		})
	}
}
