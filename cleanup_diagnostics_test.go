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
							t.Setenv("DOCKER_HOST", "")
							t.Setenv("DOCKER_CONTEXT", "")
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
	client, err := NewClient(WithClientBackend("apple"), WithClientRunner(r))
	if err != nil {
		t.Fatal(err)
	}
	f.failPrefix = "inspect"
	f.failStderr = "inspect failure for " + group
	_, err = client.PruneReuseGroup(context.Background(), group)
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
