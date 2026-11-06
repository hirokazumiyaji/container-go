package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
				eng.resolvedTarget = &dockerTargetEndpoint{rawHost: "tcp://" + host + ":2375", host: host, clientHost: host, context: "prod"}
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
