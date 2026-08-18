package container

import (
	"context"
	"testing"
)

// Values handed to CLI flags must never be able to masquerade as flags
// themselves or smuggle control characters.
func TestOptionsRejectFlagLikeAndControlValues(t *testing.T) {
	f := newTestRunner()
	cases := []struct {
		name string
		opt  Option
	}{
		{"user leading dash", WithUser("--privileged")},
		{"user newline", WithUser("root\n--privileged")},
		{"workdir leading dash", WithWorkingDir("-w")},
		{"workdir relative", WithWorkingDir("relative/dir")},
		{"network leading dash", WithNetwork("--publish-socket")},
		{"network invalid chars", WithNetwork("net work")},
		{"platform leading dash", WithPlatform("-linux")},
		{"platform invalid", WithPlatform("linux/amd64/x/y")},
		{"entrypoint leading dash", WithEntrypoint("--rm")},
		{"memory flag-like", WithMemory("--cpus")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), tc.opt, withRunner(f))
			if err == nil {
				t.Errorf("want validation error")
			}
		})
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid options: %v", f.calls)
	}
}

func TestExecOptionsRejectFlagLikeValues(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"id"}, WithExecUser("--privileged")); err == nil {
		t.Error("WithExecUser: want validation error")
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"id"}, WithExecWorkDir("-w")); err == nil {
		t.Error("WithExecWorkDir: want validation error")
	}
}

func TestImageReferenceRejectsFlagInjection(t *testing.T) {
	f := newTestRunner()
	for _, img := range []string{"", "-redis", "--privileged", "redis image"} {
		_, err := Run(context.Background(), img, WithName("myctr"), withRunner(f))
		if err == nil {
			t.Errorf("image %q: want validation error", img)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid image: %v", f.calls)
	}
}

func TestPublishedPortRejectsInvalidHostAddress(t *testing.T) {
	f := newTestRunner()
	for _, spec := range []string{"evil-host:8080:80", "--flag:8080:80"} {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithPublishedPort(spec), withRunner(f))
		if err == nil {
			t.Errorf("spec %q: want validation error", spec)
		}
	}
}

func TestPublishedPortAcceptsValidHostAddresses(t *testing.T) {
	f := newTestRunner()
	for _, spec := range []string{"127.0.0.1:8080:80", "[::1]:8080:80", "8080:80"} {
		if _, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithPublishedPort(spec), withRunner(f)); err != nil {
			t.Errorf("spec %q: unexpected error %v", spec, err)
		}
	}
}
