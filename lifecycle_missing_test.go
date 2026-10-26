package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const lifecycleTestCreation = "0123456789abcdef"

const (
	appleStartMissing = `Error: notFound: "get failed: container myctr not found"`
	appleKillMissing  = `Error: internalError: "failed to kill container" (cause: "notFound: \"container with ID myctr not found\"")`
	appleStartWrapper = `Error: internalError: "failed to start container" (cause: "notFound: \"container with ID myctr not found\"")`
	dockerMissing     = "Error response from daemon: No such container: " + dockerFixtureID
)

func newDockerLifecycleContainer(runner cli.Runner) *Container {
	return &Container{
		id:       "myctr",
		runner:   runner,
		eng:      dockerEngine{},
		creation: lifecycleTestCreation,
		sync:     &lockPack{uid: dockerFixtureID},
	}
}

func TestLifecycleMissingContainerClassification(t *testing.T) {
	ctx := context.Background()

	dockerCases := []struct {
		name       string
		failPrefix string
		run        func(*Container) error
	}{
		{name: "start", failPrefix: "start", run: func(c *Container) error { return c.Start(ctx) }},
		{name: "restart", failPrefix: "restart", run: func(c *Container) error { return c.Restart(ctx, nil) }},
		{name: "restart timeout", failPrefix: "restart", run: func(c *Container) error {
			timeout := 5 * time.Second
			return c.Restart(ctx, &timeout)
		}},
		{name: "kill", failPrefix: "kill", run: func(c *Container) error { return c.Kill(ctx, "") }},
		{name: "kill signal", failPrefix: "kill", run: func(c *Container) error { return c.Kill(ctx, "SIGTERM") }},
		{name: "pause", failPrefix: "pause", run: func(c *Container) error { return c.Pause(ctx) }},
		{name: "unpause", failPrefix: "unpause", run: func(c *Container) error { return c.Unpause(ctx) }},
	}
	for _, tc := range dockerCases {
		t.Run("docker "+tc.name, func(t *testing.T) {
			f := newDockerTestRunner()
			f.failPrefix = tc.failPrefix
			f.failStderr = dockerMissing
			ctr := newDockerLifecycleContainer(f)

			err := tc.run(ctr)
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("%s error = %v, want ErrContainerNotFound", tc.name, err)
			}
		})
	}

	appleCases := []struct {
		name       string
		failPrefix string
		failStderr string
		run        func(*Container) error
	}{
		{name: "start", failPrefix: "start", failStderr: appleStartMissing,
			run: func(c *Container) error { return c.Start(ctx) }},
		{name: "restart", failPrefix: "start", failStderr: appleStartMissing,
			run: func(c *Container) error { return c.Restart(ctx, nil) }},
		{name: "kill", failPrefix: "kill", failStderr: appleKillMissing,
			run: func(c *Container) error { return c.Kill(ctx, "") }},
		{name: "kill signal", failPrefix: "kill", failStderr: appleKillMissing,
			run: func(c *Container) error { return c.Kill(ctx, "SIGTERM") }},
	}
	for _, tc := range appleCases {
		t.Run("apple "+tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.failPrefix = tc.failPrefix
			f.failStderr = tc.failStderr

			err := tc.run(ctr)
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("%s error = %v, want ErrContainerNotFound", tc.name, err)
			}
		})
	}
}

func TestLifecycleCommandTargetExtraction(t *testing.T) {
	cases := []struct {
		command string
		args    []string
		want    string
	}{
		{command: "start", args: []string{"myctr"}, want: "myctr"},
		{command: "restart", args: []string{"-t", "5", "myctr"}, want: "myctr"},
		{command: "restart", args: []string{"myctr", "-t", "5"}, want: "myctr"},
		{command: "kill", args: []string{"-s", "SIGTERM", "myctr"}, want: "myctr"},
		{command: "kill", args: []string{"--signal", "SIGTERM", "myctr"}, want: "myctr"},
		{command: "kill", args: []string{"myctr", "--signal", "SIGTERM"}, want: "myctr"},
		{command: "pause", args: []string{"myctr"}, want: "myctr"},
		{command: "unpause", args: []string{"myctr"}, want: "myctr"},
		// -t and -s are boolean shorthands for these read commands, so
		// they must not consume the container operand.
		{command: "logs", args: []string{"-t", "myctr"}, want: "myctr"},
		{command: "exec", args: []string{"-t", "myctr", "sh"}, want: "myctr"},
		{command: "inspect", args: []string{"-s", "myctr"}, want: "myctr"},
	}
	for _, tc := range cases {
		t.Run(tc.command+" "+tc.args[0], func(t *testing.T) {
			if got := cliCommandTarget(tc.command, tc.args); got != tc.want {
				t.Fatalf("cliCommandTarget(%q, %q) = %q, want %q", tc.command, tc.args, got, tc.want)
			}
		})
	}
}

func TestAppleLifecycleMissingForms(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		stderr    string
		want      bool
	}{
		{name: "start get failed", operation: "start", stderr: appleStartMissing, want: true},
		{name: "start typed wrapper", operation: "start", stderr: appleStartWrapper, want: true},
		{name: "kill typed wrapper", operation: "kill", stderr: appleKillMissing, want: true},
		{name: "start generic not found", operation: "start",
			stderr: `Error: notFound: "container not found: myctr"`, want: true},
		{name: "start flat wrapper", operation: "start",
			stderr: "Error: failed to start container: container with ID myctr not found", want: true},
		{name: "kill flat wrapper", operation: "kill",
			stderr: "Error: failed to kill container: container with ID myctr not found", want: true},
		{name: "start flat get failed", operation: "start",
			stderr: "Error: get failed: container myctr not found", want: true},
		{name: "start wrong target", operation: "start",
			stderr: `Error: internalError: "failed to start container" (cause: "notFound: \"container with ID other not found\"")`, want: false},
		{name: "kill application output", operation: "kill",
			stderr: `Error: application: notFound: "container not found: myctr"`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &cli.CLIError{
				Binary:   "container",
				Args:     []string{tc.operation, "myctr"},
				ExitCode: 1,
				Stderr:   tc.stderr,
			}
			if got := (appleEngine{}).containerMissing(err); got != tc.want {
				t.Fatalf("containerMissing(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

func TestLifecycleUsesEffectiveQueryTimeout(t *testing.T) {
	const customTimeout = 45 * time.Second
	ctx := context.Background()

	recordDeadline := func(observed *time.Time) cli.Runner {
		return timeoutTestRunner(func(ctx context.Context, _ ...string) ([]byte, []byte, error) {
			if observed.IsZero() {
				*observed, _ = ctx.Deadline()
			}
			return nil, nil, errors.New("simulated backend failure")
		})
	}
	assertDeadline := func(t *testing.T, observed time.Time, started time.Time, name string) {
		t.Helper()
		if observed.IsZero() {
			t.Fatalf("%s did not reach the backend with a deadline", name)
		}
		remaining := observed.Sub(started)
		if remaining < customTimeout-time.Second || remaining > customTimeout+time.Second {
			t.Fatalf("%s deadline = %v away, want ~%v", name, remaining, customTimeout)
		}
	}

	dockerCases := []struct {
		name string
		run  func(*Container) error
	}{
		{name: "start", run: func(c *Container) error { return c.Start(ctx) }},
		{name: "restart", run: func(c *Container) error { return c.Restart(ctx, nil) }},
		{name: "kill", run: func(c *Container) error { return c.Kill(ctx, "") }},
		{name: "pause", run: func(c *Container) error { return c.Pause(ctx) }},
		{name: "unpause", run: func(c *Container) error { return c.Unpause(ctx) }},
	}
	for _, tc := range dockerCases {
		t.Run("docker "+tc.name, func(t *testing.T) {
			var observed time.Time
			ctr := newDockerLifecycleContainer(recordDeadline(&observed))
			ctr.queryTimeout = customTimeout
			started := time.Now()
			_ = tc.run(ctr)
			assertDeadline(t, observed, started, tc.name)
		})
	}

	appleCases := []struct {
		name string
		run  func(*Container) error
	}{
		{name: "start", run: func(c *Container) error { return c.Start(ctx) }},
		{name: "restart", run: func(c *Container) error { return c.Restart(ctx, nil) }},
		{name: "kill", run: func(c *Container) error { return c.Kill(ctx, "") }},
	}
	for _, tc := range appleCases {
		t.Run("apple "+tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			var observed time.Time
			ctr.runner = recordDeadline(&observed)
			ctr.queryTimeout = customTimeout
			started := time.Now()
			_ = tc.run(ctr)
			assertDeadline(t, observed, started, tc.name)
		})
	}
}
