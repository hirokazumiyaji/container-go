package wait

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// optionsExecTarget implements ExecOptionsTarget and records each call so the
// tests can assert how ForExec bridged its execution options.
type optionsExecTarget struct {
	*fakeTarget
	mu          sync.Mutex
	plainCalls  int
	outputCalls int
	options     []ExecOptions
	results     []execResult
	fallback    execResult
}

func newOptionsExecTarget(results ...execResult) *optionsExecTarget {
	target := &optionsExecTarget{fakeTarget: newFakeTarget(), results: results}
	if len(results) > 0 {
		target.fallback = results[len(results)-1]
	}
	return target
}

func (t *optionsExecTarget) next() execResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.results) == 0 {
		return t.fallback
	}
	result := t.results[0]
	t.results = t.results[1:]
	return result
}

func (t *optionsExecTarget) record(options ExecOptions) {
	t.mu.Lock()
	defer t.mu.Unlock()
	copied := options
	if len(options.Env) > 0 {
		copied.Env = make(map[string]string, len(options.Env))
		for key, value := range options.Env {
			copied.Env[key] = value
		}
	}
	t.options = append(t.options, copied)
}

func (t *optionsExecTarget) ExecCommandWithOptions(_ context.Context, _ []string, options ExecOptions) (int, error) {
	t.record(options)
	t.mu.Lock()
	t.plainCalls++
	t.mu.Unlock()
	result := t.next()
	return result.code, result.err
}

func (t *optionsExecTarget) ExecCommandOutputWithOptions(_ context.Context, _ []string, options ExecOptions) (int, []byte, error) {
	t.record(options)
	t.mu.Lock()
	t.outputCalls++
	t.mu.Unlock()
	result := t.next()
	return result.code, []byte(result.output), result.err
}

// ExecCommandOutput satisfies ExecOutputTarget so an output matcher accepts
// this target. The options-probe tests assert that this legacy surface is not
// used when execution options are configured.
func (t *optionsExecTarget) ExecCommandOutput(context.Context, []string) (int, []byte, error) {
	result := t.next()
	return result.code, []byte(result.output), result.err
}

func (t *optionsExecTarget) calls() (plain, output int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.plainCalls, t.outputCalls
}

func (t *optionsExecTarget) recordedOptions() []ExecOptions {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ExecOptions(nil), t.options...)
}

func TestForExecPassesUserWorkDirAndEnv(t *testing.T) {
	target := newOptionsExecTarget(execResult{})
	err := ForExec([]string{"pg_isready"}).
		WithUser("app").
		WithWorkDir("/srv/app").
		WithEnv(map[string]string{"PGPASSWORD": "secret"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	plain, output := target.calls()
	if plain != 1 || output != 0 {
		t.Errorf("plain calls = %d, output calls = %d; want the plain options path", plain, output)
	}
	options := target.recordedOptions()
	if len(options) != 1 {
		t.Fatalf("recorded options = %v, want one call", options)
	}
	got := options[0]
	if got.User != "app" || got.WorkDir != "/srv/app" || got.Env["PGPASSWORD"] != "secret" {
		t.Errorf("options = %+v, want the configured user, workdir, and env", got)
	}
}

func TestForExecPassesOptionsToOutputProbe(t *testing.T) {
	target := newOptionsExecTarget(execResult{output: "READY\n"})
	err := ForExec([]string{"probe"}).
		WithUser("app").
		WithEnv(map[string]string{"TOKEN": "secret"}).
		WithOutputContains("READY").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	plain, output := target.calls()
	if output != 1 || plain != 0 {
		t.Errorf("plain calls = %d, output calls = %d; want the output options path", plain, output)
	}
	options := target.recordedOptions()
	if len(options) != 1 || options[0].User != "app" {
		t.Fatalf("recorded options = %v, want the output probe options", options)
	}
}

func TestForExecWithOptionsRequiresExecOptionsTarget(t *testing.T) {
	start := time.Now()
	for _, strategy := range []*ExecStrategy{
		ForExec([]string{"probe"}).WithUser("app"),
		ForExec([]string{"probe"}).WithWorkDir("/srv"),
		ForExec([]string{"probe"}).WithEnv(map[string]string{"TOKEN": "secret"}),
	} {
		err := strategy.
			WithStartupTimeout(time.Second).
			WaitUntilReady(context.Background(), newFakeTarget())
		if err == nil {
			t.Fatal("WaitUntilReady succeeded without an ExecOptionsTarget")
		}
		if !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("error = %v, want ErrInvalidConfiguration", err)
		}
		var configErr *ConfigError
		if !errors.As(err, &configErr) || configErr.Strategy != "ForExec" || configErr.Field != "target" {
			t.Errorf("error = %v, want the target ConfigError", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want immediate failures", elapsed)
	}
}

func TestForExecEnvValuesAreDiagnosticSecrets(t *testing.T) {
	const secret = "exec-environment-secret-117"
	strategy := ForExec([]string{"probe"}).WithEnv(map[string]string{"TOKEN": secret})
	values := strings.Join(DiagnosticValues(strategy), "\x00")
	if !strings.Contains(values, secret) {
		t.Fatalf("DiagnosticValues() = %q, want the environment value", values)
	}

	target := newOptionsExecTarget(execResult{err: errors.New("backend rejected " + secret)})
	err := strategy.
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains the environment secret", err)
	}
}

func TestForExecWithEnvCopiesAndMergesMaps(t *testing.T) {
	caller := map[string]string{"TOKEN": "one"}
	base := ForExec([]string{"probe"}).WithEnv(caller)
	caller["TOKEN"] = "mutated"
	caller["EXTRA"] = "mutated"

	if got := base.env["TOKEN"]; got != "one" {
		t.Fatalf("strategy env TOKEN = %q, want the copied value", got)
	}
	if _, ok := base.env["EXTRA"]; ok {
		t.Fatal("strategy shares the caller's environment map")
	}

	merged := base.WithEnv(map[string]string{"OTHER": "two"})
	if merged.env["TOKEN"] != "one" || merged.env["OTHER"] != "two" {
		t.Fatalf("merged env = %v, want both values", merged.env)
	}
	if _, ok := base.env["OTHER"]; ok {
		t.Fatal("WithEnv mutated the receiver")
	}

	sibling := merged.WithEnv(map[string]string{"SIBLING": "three"})
	merged.env["LOCAL"] = "value"
	if _, ok := sibling.env["LOCAL"]; ok {
		t.Fatal("derived strategies share one environment map")
	}
	if sibling.env["SIBLING"] != "three" || sibling.env["TOKEN"] != "one" {
		t.Fatalf("sibling env = %v, want inherited and added values", sibling.env)
	}
}

func TestForExecOptionsBuildersDoNotMutateReceiver(t *testing.T) {
	base := ForExec([]string{"probe"})
	derived := base.WithUser("app").
		WithWorkDir("/srv").
		WithEnv(map[string]string{"TOKEN": "secret"})

	if base.user != "" || base.workDir != "" || len(base.env) != 0 {
		t.Errorf("base strategy was mutated: %+v", base)
	}
	if derived.user != "app" || derived.workDir != "/srv" || derived.env["TOKEN"] != "secret" {
		t.Errorf("derived strategy = %+v", derived)
	}
}
