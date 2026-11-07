package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type pruneDiagnosticRunner struct {
	*fakeRunner
	list          string
	listOperation string
}

func (r *pruneDiagnosticRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == r.listOperation && r.failPrefix != r.listOperation {
		return []byte(r.list), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestClientPruneRedactsDockerTargetDiagnostics(t *testing.T) {
	const host = "tcp://private-prune-daemon:2375"
	const dockerContext = "private-prune-context"
	const configDir = "/private/prune-settings-dir"
	const group = "private-prune-group"
	for _, source := range []string{"options", "environment"} {
		for _, target := range []string{"host", "context"} {
			for _, grouped := range []bool{false, true} {
				for _, operation := range []string{"ps", "inspect", "rm"} {
					t.Run(fmt.Sprintf("%s/%s/group=%v/%s", source, target, grouped, operation), func(t *testing.T) {
						t.Setenv("DOCKER_HOST", "")
						t.Setenv("DOCKER_CONTEXT", "")
						t.Setenv("DOCKER_CONFIG", "")
						f := newTestRunner()
						f.binary = "docker"
						f.inspectJSON = fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}}}]`, cleanupDockerIDOne, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, group)
						r := &pruneDiagnosticRunner{fakeRunner: f, list: cleanupDockerIDOne + "\n", listOperation: "ps"}
						opts := []ClientOption{WithClientBackend("docker"), WithClientRunner(r)}
						if source == "options" {
							opts = append(opts, WithClientDockerConfig(configDir))
							if target == "host" {
								opts = append(opts, WithClientDockerHost(host))
							} else {
								opts = append(opts, WithClientDockerContext(dockerContext))
							}
						} else {
							if target == "host" {
								t.Setenv("DOCKER_HOST", host)
							} else {
								t.Setenv("DOCKER_CONTEXT", dockerContext)
							}
							t.Setenv("DOCKER_CONFIG", configDir)
						}
						client, err := NewClient(opts...)
						if err != nil {
							t.Fatal(err)
						}
						f.failPrefix = operation
						f.failStderr = strings.Join([]string{"prune diagnostic", host, dockerContext, configDir, group}, " ")
						if grouped {
							_, err = client.PruneReuseGroup(context.Background(), group)
						} else {
							_, err = client.Prune(context.Background())
						}
						if err == nil {
							t.Fatal("expected CLI failure")
						}
						secrets := []string{configDir}
						if target == "host" {
							secrets = append(secrets, host)
						} else {
							secrets = append(secrets, dockerContext)
						}
						if grouped {
							secrets = append(secrets, group)
						}
						assertPruneDiagnosticSecrets(t, err, secrets...)
					})
				}
			}
		}
	}

}

func TestClientPruneReuseGroupRedactsAppleInspectDiagnostics(t *testing.T) {
	const group = "private-apple-prune-group"
	f := newTestRunner()
	r := &pruneDiagnosticRunner{fakeRunner: f, listOperation: "ls", list: fmt.Sprintf(`[{"id":"private-apple-candidate","configuration":{"labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}},"status":{"state":"stopped"}}]`, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, group)}
	client := &Client{eng: appleEngine{}, runner: r}
	f.failPrefix = "inspect"
	f.failStderr = "inspect failure for " + group
	_, err := client.PruneReuseGroup(context.Background(), group)
	if err == nil {
		t.Fatal("expected inspect failure")
	}
	assertPruneDiagnosticSecrets(t, err, group)
}

func assertPruneDiagnosticSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("public error contains %q: %v", secret, err)
		}
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error lost CLIError: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(cliErr.Error(), secret) {
			t.Errorf("CLIError contains %q: %v", secret, cliErr)
		}
	}
}

type canceledPruneDiagnosticRunner struct {
	cancel context.CancelFunc
	group  string
}

func (r canceledPruneDiagnosticRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.cancel()
	return nil, nil, errors.Join(fmt.Errorf("docker %s %s: %w", strings.Join(args, " "), r.group, context.Canceled), &cli.CLIError{Binary: "docker", Args: args, Stderr: "failure at private-cancel-daemon"})
}
func TestPruneCancellationRedactsDiagnosticContext(t *testing.T) {
	const group = "cancel-secret-group"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &Client{eng: dockerEngine{host: "tcp://private-cancel-daemon:2375", explicitHost: true}, runner: canceledPruneDiagnosticRunner{cancel: cancel, group: group}}
	_, err := client.PruneReuseGroup(ctx, group)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	assertPruneDiagnosticSecrets(t, err, group, "private-cancel-daemon")
}

type envPruneDiagnosticRunner struct {
	*pruneDiagnosticRunner
	env []string
}

func (r envPruneDiagnosticRunner) ExternalEnv() []string { return r.env }
func TestPruneRedactsResolvedAndRunnerTargets(t *testing.T) {
	for _, source := range []string{"resolved", "runner"} {
		t.Run(source, func(t *testing.T) {
			const host = "private-resolved-daemon"
			const configDir = "/private/runner-config"
			f := newTestRunner()
			f.binary = "docker"
			f.failPrefix = "ps"
			f.failStderr = "Cannot connect to http://" + host + ":2375: lookup " + host + "; config " + configDir
			r := envPruneDiagnosticRunner{pruneDiagnosticRunner: &pruneDiagnosticRunner{fakeRunner: f, listOperation: "ps"}}
			eng := dockerEngine{context: "prod", explicitContext: true}
			if source == "resolved" {
				eng.resolvedTarget = &dockerTargetEndpoint{rawHost: "tcp://" + host + ":2375", host: host, clientHost: host, context: "prod", isRemote: true}
				eng.configDir = configDir
			} else {
				r.env = []string{"DOCKER_HOST=tcp://" + host + ":2375", "DOCKER_CONFIG=" + configDir}
			}
			client := &Client{eng: eng, runner: r}
			_, err := client.Prune(context.Background())
			if err == nil {
				t.Fatal("expected failure")
			}
			assertPruneDiagnosticSecrets(t, err, host, configDir)
		})
	}
}

func TestPruneAppleListAndDeleteRedaction(t *testing.T) {
	for _, op := range []string{"ls", "delete"} {
		t.Run(op, func(t *testing.T) {
			const group = "private-apple-prune-group"
			f := newTestRunner()
			f.failPrefix = op
			f.failStderr = "failure " + group
			f.inspectJSON = fmt.Sprintf(`[{"id":"private-apple-candidate","configuration":{"labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}},"status":{"state":"stopped"}}]`, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, group)
			r := &pruneDiagnosticRunner{fakeRunner: f, listOperation: "ls", list: f.inspectJSON}
			_, err := (&Client{eng: appleEngine{}, runner: r}).PruneReuseGroup(context.Background(), group)
			if err == nil {
				t.Fatal("expected failure")
			}
			assertPruneDiagnosticSecrets(t, err, group)
		})
	}
}

func TestPruneKeepsDefaultContextDiagnosticText(t *testing.T) {
	f := newTestRunner()
	f.binary = "docker"
	f.failPrefix = "ps"
	f.failStderr = "default daemon unavailable"
	client := &Client{eng: dockerEngine{context: "default", explicitContext: true}, runner: f}
	_, err := client.Prune(context.Background())
	if err == nil || !strings.Contains(err.Error(), "default daemon unavailable") {
		t.Fatalf("default context changed diagnostic: %v", err)
	}
}

func TestDockerLocalTargetDiagnosticsStayReadable(t *testing.T) {
	for _, raw := range []string{"unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "tcp://localhost:2375"} {
		t.Run(raw, func(t *testing.T) {
			ep, err := parseDockerEndpoint(raw)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCKER_HOST", "")
			cfg := &config{eng: dockerEngine{resolvedTarget: &ep}}
			input := "Cannot connect to the Docker daemon at " + raw + ". dial 127.0.0.1:5000: connection refused"
			got := cfg.publicError(&cli.CLIError{Binary: "docker", Args: []string{"ps"}, ExitCode: 1, Stderr: input}).Error()
			if !strings.Contains(got, input) {
				t.Fatalf("local diagnostic changed: %s", got)
			}
		})
	}
}

func TestDockerGenericHostnameDoesNotCorruptDiagnostics(t *testing.T) {
	for _, host := range []string{"docker", "db"} {
		t.Run(host, func(t *testing.T) {
			raw := "tcp://" + host + ":2375"
			cfg := &config{eng: dockerEngine{host: raw, explicitHost: true}}
			input := "docker daemon debug database dbus failure at " + raw
			got := cfg.publicError(&cli.CLIError{Binary: "docker", Args: []string{"ps"}, ExitCode: 1, Stderr: input}).Error()
			if !strings.Contains(got, "debug database dbus failure at ") {
				t.Fatalf("generic hostname corrupted diagnostic: %s", got)
			}
			if strings.Contains(got, raw) {
				t.Fatalf("raw remote target leaked: %s", got)
			}
		})
	}
}

func TestPackagePruneRedactsResolvedContextEndpoint(t *testing.T) {
	for _, source := range []string{"environment", "active", "runner"} {
		for _, grouped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/group=%v", source, grouped), func(t *testing.T) {
				t.Setenv("CONTAINERGO_BACKEND", "docker")
				t.Setenv("DOCKER_HOST", "")
				t.Setenv("DOCKER_CONTEXT", "")
				t.Setenv("DOCKER_CONFIG", "")
				const host = "private-package-prune-daemon"
				contextName := "private-prune-context"
				f := newTestRunner()
				f.binary = "docker"
				f.contextJSON = fmt.Sprintf(`[{"Name":%q,"Endpoints":{"docker":{"Host":"tcp://%s:2375"}}}]`, contextName, host)
				f.failPrefix = "ps"
				f.failStderr = "failure from http://" + host + ":2375: lookup " + host + " in " + contextName
				r := envPruneDiagnosticRunner{pruneDiagnosticRunner: &pruneDiagnosticRunner{fakeRunner: f, listOperation: "ps"}}
				if source == "environment" {
					t.Setenv("DOCKER_CONTEXT", contextName)
				}
				if source == "runner" {
					r.env = []string{"DOCKER_CONTEXT=" + contextName}
				}
				var err error
				if grouped {
					_, err = PruneReuseGroup(context.Background(), "private-prune-group", WithPruneRunner(r))
				} else {
					_, err = Prune(context.Background(), WithPruneRunner(r))
				}
				if err == nil {
					t.Fatal("expected list failure")
				}
				assertPruneDiagnosticSecrets(t, err, host, contextName)
			})
		}
	}
}

func TestPruneContextResolutionUsesQueryAndOverallBudgets(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		query, overall, want time.Duration
	}{
		{name: "default", overall: time.Hour, want: queryTimeout},
		{name: "configured", query: 5 * time.Second, overall: time.Hour, want: 5 * time.Second},
		{name: "overall", query: time.Minute, overall: time.Second, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", "")
			t.Setenv("DOCKER_CONTEXT", "")
			var remaining time.Duration
			r := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
				if args[0] != "context" {
					t.Fatalf("unexpected operation: %v", args)
				}
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("missing context lookup deadline")
				}
				remaining = time.Until(deadline)
				return []byte(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
			})
			cfg := pruneConfig{timeout: tc.overall, queryTimeout: tc.query, runner: r}
			if _, _, err := cfg.prepareTarget(context.Background(), dockerEngine{}, ""); err != nil {
				t.Fatal(err)
			}
			if remaining <= 0 || remaining > tc.want {
				t.Fatalf("remaining=%v, want <=%v", remaining, tc.want)
			}
		})
	}
}

func TestDockerLocalTargetCredentialsAreRedacted(t *testing.T) {
	const raw = "tcp://private-local-token@localhost:2375"
	cfg := &config{eng: dockerEngine{host: raw, explicitHost: true}, dockerHost: new(string)}
	*cfg.dockerHost = raw
	input := "Cannot connect at " + raw + ". dial 127.0.0.1:5000: connection refused"
	got := cfg.publicError(&cli.CLIError{Binary: "docker", Args: []string{"ps"}, ExitCode: 1, Stderr: input}).Error()
	if strings.Contains(got, "private-local-token") {
		t.Fatalf("local credentials leaked: %s", got)
	}
	if !strings.Contains(got, "dial 127.0.0.1:5000: connection refused") {
		t.Fatalf("local address lost: %s", got)
	}
}

func TestPackagePruneTargetResolutionSharesOverallBudget(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%v", grouped), func(t *testing.T) {
			t.Setenv("CONTAINERGO_BACKEND", "docker")
			t.Setenv("DOCKER_HOST", "")
			t.Setenv("DOCKER_CONTEXT", "")
			var lookupDeadline, listDeadline time.Time
			r := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("missing deadline")
				}
				switch args[0] {
				case "context":
					lookupDeadline = deadline
					return []byte(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
				case "ps":
					listDeadline = deadline
					return nil, nil, nil
				default:
					t.Fatalf("unexpected operation: %v", args)
					return nil, nil, nil
				}
			})
			opts := []PruneOption{WithPruneRunner(r), WithPruneTimeout(5 * time.Second)}
			var err error
			if grouped {
				_, err = PruneReuseGroup(context.Background(), "review-budget", opts...)
			} else {
				_, err = Prune(context.Background(), opts...)
			}
			if err != nil {
				t.Fatal(err)
			}
			if lookupDeadline.IsZero() || !lookupDeadline.Equal(listDeadline) {
				t.Fatalf("lookup deadline=%v list deadline=%v, want one shared overall budget", lookupDeadline, listDeadline)
			}
		})
	}
}

func TestExplicitLocalDockerEndpointIsRedacted(t *testing.T) {
	for _, raw := range []string{"unix:///home/alice/acme-widgets/docker.sock", "tcp://127.0.0.1:12345"} {
		cfg := &config{dockerHost: &raw, eng: dockerEngine{host: raw, explicitHost: true}}
		if got := cfg.diagnosticRedactor().Text("endpoint " + raw); strings.Contains(got, raw) {
			t.Fatalf("explicit target leaked: %s", got)
		}
	}
}

func TestShortDockerHostnameIsBoundaryRedacted(t *testing.T) {
	for _, host := range []string{"finance1", "db"} {
		raw := "tcp://" + host + ":2375"
		cfg := &config{eng: dockerEngine{host: raw, explicitHost: true}}
		got := cfg.diagnosticRedactor().Text("lookup " + host + ": no such host; x" + host + "y database dbus debug")
		if strings.Contains(got, "lookup "+host+":") || !strings.Contains(got, "x"+host+"y database dbus debug") {
			t.Fatalf("host boundary redaction: %s", got)
		}
	}
}

func TestPackagePruneImplicitContextLookupIsBestEffort(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	r := timeoutTestRunner(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "context" {
			return nil, []byte("failed for private-ctx"), &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "failed for private-ctx"}
		}
		if args[0] == "ps" {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected command: %v", args)
	})
	if removed, err := Prune(context.Background(), WithPruneRunner(r)); err != nil || len(removed) != 0 {
		t.Fatalf("Prune=%v,%v", removed, err)
	}
}

func TestPruneUnavailableContextPreservesCause(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "private-ctx")
	r := timeoutTestRunner(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		return nil, []byte("context private-ctx: executable file not found"), &exec.Error{Name: "docker", Err: exec.ErrNotFound}
	})
	_, err := Prune(context.Background(), WithPruneRunner(r))
	if !errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "private-ctx") {
		t.Fatalf("Prune cause/redaction=%v", err)
	}
}

func TestPruneRunnerConfigStoreStaysPinned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell stub")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", "/ambient-zz")
	dir := t.TempDir()
	binary := filepath.Join(dir, "docker-stub")
	log := filepath.Join(dir, "env.log")
	script := `#!/bin/sh
printf '%s %s\n' "$1" "${DOCKER_CONFIG-unset}" >> "$PRUNE_ENV_LOG"
case "$1" in
context) printf '%s\n' '[{"Name":"runner-context","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]' ;;
ps) ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"DOCKER_CONFIG=/runner-zz", "DOCKER_CONFIG"} {
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		r := &cli.ExecRunner{Binary: binary, Env: []string{setting, "PRUNE_ENV_LOG=" + log}}
		if _, err := Prune(context.Background(), WithPruneRunner(r)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		value := "/runner-zz"
		if setting == "DOCKER_CONFIG" {
			value = "unset"
		}
		want := "context " + value + "\nps " + value + "\n"
		if string(got) != want {
			t.Fatalf("child config stores=%q, want %q", got, want)
		}
	}
}

func TestPruneContextLookupBudgetsKeepFailurePolicy(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	r := timeoutTestRunner(func(ctx context.Context, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := pruneConfig{timeout: time.Second, queryTimeout: time.Millisecond, runner: r}
	if _, _, err := cfg.prepareTarget(ctx, dockerEngine{}, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("explicit budget=%v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	cfg.queryTimeout = 0
	if _, _, err := cfg.prepareTarget(canceled, dockerEngine{}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation=%v", err)
	}
	cfg.runner = timeoutTestRunner(func(context.Context, ...string) ([]byte, []byte, error) { return nil, nil, context.DeadlineExceeded })
	if _, _, err := cfg.prepareTarget(context.Background(), dockerEngine{}, ""); err != nil {
		t.Fatalf("implicit unavailable lookup=%v", err)
	}
}

func TestPruneRemovedContextKeepsHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "tcp://host-a:2375")
	dir := t.TempDir()
	binary, log := filepath.Join(dir, "docker"), filepath.Join(dir, "env.log")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$DOCKER_HOST\" >> \"$PRUNE_ENV_LOG\"\n[ \"$1\" = ps ]\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner := &cli.ExecRunner{Binary: binary, Env: []string{"PRUNE_ENV_LOG=" + log}}
	client, err := NewClient(WithClientRunner(runner))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "tcp://host-b:2375")
	if _, err := client.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.Env = append(runner.Env, "DOCKER_HOST=tcp://host-c:2375", "DOCKER_CONTEXT=removed-context", "DOCKER_CONTEXT")
	if _, err := Prune(context.Background(), WithPruneRunner(runner)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ps tcp://host-a:2375\nps tcp://host-c:2375\n" {
		t.Fatalf("targets=%q", got)
	}
}

func TestPruneImplicitEmptyContextIsBestEffort(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	for _, output := range []string{"[]", `[{"Name":"private-ctx","Endpoints":{"docker":{"Host":"tcp://private host:2375"}}}]`} {
		r := timeoutTestRunner(func(_ context.Context, args ...string) ([]byte, []byte, error) {
			if args[0] == "context" {
				return []byte(output), nil, nil
			}
			return nil, nil, nil
		})
		if _, err := Prune(context.Background(), WithPruneRunner(r)); err != nil {
			t.Fatalf("output=%s: %v", output, err)
		}
	}
}

func TestPruneMalformedExplicitContextEndpointIsRedacted(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_CONTEXT", "private-ctx")
	r := timeoutTestRunner(func(context.Context, ...string) ([]byte, []byte, error) {
		return []byte(`[{"Endpoints":{"docker":{"Host":"tcp://private host:2375"}}}]`), nil, nil
	})
	_, err := Prune(context.Background(), WithPruneRunner(r))
	var configErr *ConfigError
	if !errors.As(err, &configErr) || strings.Contains(err.Error(), "private host") || strings.Contains(configErr.Detail, "private host") {
		t.Fatalf("unsafe error=%v (%+v)", err, configErr)
	}
}

func TestDockerBinaryHostnameRemainsReadable(t *testing.T) {
	de := newDockerEngine("tcp://docker:2375", "", "", "")
	err := cli.WithRedactor(&cli.CLIError{Binary: "docker", Args: []string{"ps"}, Stderr: "daemon docker tcp://docker:2375 failed"}, pruneDiagnosticRedactor(de, nil, ""))
	var commandErr *cli.CLIError
	if !errors.As(err, &commandErr) || commandErr.Binary != "docker" || !strings.Contains(err.Error(), "docker ps") || strings.Contains(err.Error(), "tcp://docker:2375") || strings.Contains(commandErr.Error(), "tcp://docker:2375") {
		t.Fatalf("binary diagnostic=%v (%+v)", err, commandErr)
	}
}

func TestPruneConfiguredContextFailurePreservesRedactedCause(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_CONTEXT", "private-ctx")
	r := timeoutTestRunner(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		return nil, nil, &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "private-ctx failed"}
	})
	_, err := Prune(context.Background(), WithPruneRunner(r))
	var commandErr *cli.CLIError
	var configErr *ConfigError
	if !errors.As(err, &commandErr) || !errors.As(err, &configErr) || strings.Contains(err.Error(), "private-ctx") || strings.Contains(commandErr.Error(), "private-ctx") {
		t.Fatalf("cause=%v (%+v)", err, commandErr)
	}
}

func TestPackagePrunePreservesRunnerTargetEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "tcp://ambient-host:2375")
	t.Setenv("DOCKER_CONTEXT", "ambient-context")
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"ambient", nil, "tcp://ambient-host:2375 ambient-context\n"},
		{"removed-context", []string{"DOCKER_CONTEXT"}, "tcp://ambient-host:2375 unset\n"},
		{"removed-host", []string{"DOCKER_HOST"}, "unset ambient-context\n"},
	} {
		for _, grouped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/group=%v", tc.name, grouped), func(t *testing.T) {
				dir := t.TempDir()
				binary, log := filepath.Join(dir, "docker"), filepath.Join(dir, "env.log")
				script := `#!/bin/sh
case "$1" in
context)
  [ -z "$DOCKER_HOST" ] || exit 1
  printf '[{"Name":"ambient-context","Endpoints":{"docker":{"Host":"tcp://context-host:2375"}}}]'
  ;;
ps) printf '%s %s\n' "${DOCKER_HOST-unset}" "${DOCKER_CONTEXT-unset}" >> "$PRUNE_ENV_LOG" ;;
*) exit 1 ;;
esac
`
				if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				runner := &cli.ExecRunner{Binary: binary, Env: append([]string{"PRUNE_ENV_LOG=" + log}, tc.env...)}
				var err error
				if grouped {
					_, err = PruneReuseGroup(context.Background(), "test-group", WithPruneRunner(runner))
				} else {
					_, err = Prune(context.Background(), WithPruneRunner(runner))
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.want {
					t.Fatalf("target environment=%q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestPackagePruneAmbientContextCommandUnavailableIsBestEffort(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "private-ctx")
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%v", grouped), func(t *testing.T) {
			listed := false
			r := timeoutTestRunner(func(_ context.Context, args ...string) ([]byte, []byte, error) {
				switch args[0] {
				case "context":
					return nil, []byte("unknown command: docker context"), &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "unknown command: docker context"}
				case "ps":
					listed = true
					return nil, nil, nil
				default:
					return nil, nil, fmt.Errorf("unexpected command: %v", args)
				}
			})
			var err error
			if grouped {
				_, err = PruneReuseGroup(context.Background(), "review-group", WithPruneRunner(r))
			} else {
				_, err = Prune(context.Background(), WithPruneRunner(r))
			}
			if err != nil || !listed {
				t.Fatalf("listed=%v, err=%v", listed, err)
			}
		})
	}
}
