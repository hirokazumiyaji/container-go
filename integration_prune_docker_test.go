//go:build integration

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func requireDockerPruneIntegration(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "docker" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Docker integration", backend)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
}

func runDockerPruneCommand(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func inspectDockerContainerState(name string) (string, bool, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", name).CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), true, nil
	}
	// Docker prints "Error: No such object: <name>" and exits 1 for a
	// missing container; anything else is a real inspect failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(strings.ToLower(string(out)), "no such") {
		return "", false, nil
	}
	return "", false, fmt.Errorf("docker inspect %s: %w: %s", name, err, out)
}

func waitDockerContainerState(t *testing.T, name, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, found, err := inspectDockerContainerState(name)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if found && state == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	state, found, err := inspectDockerContainerState(name)
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	t.Fatalf("state of %s = %q (found=%t), want %q", name, state, found, want)
}

// isolatedDockerPruneRunner injects an extra label filter so a live-daemon
// Prune only touches this test's fixtures; production Prune remains
// eligible to remove managed containers from any session.
type isolatedDockerPruneRunner struct {
	runner      cli.Runner
	labelFilter string
	calls       [][]string
}

func (r *isolatedDockerPruneRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		args = append(append([]string(nil), args...), "--filter", r.labelFilter)
	}
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.runner.Run(ctx, args...)
}

// TestIntegrationDockerPruneExactFixtures covers the state boundary of the
// shared Prune core while keeping the live-daemon test hermetic.
func TestIntegrationDockerPruneExactFixtures(t *testing.T) {
	requireDockerPruneIntegration(t)
	ctx := context.Background()

	if err := pullWith(ctx, &cli.ExecRunner{Binary: "docker"}, dockerEngine{}, integrationAlpine); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	labelKey := "com.github.hirokazumiyaji.container-go.test.prune"
	fixtureLabel := labelKey + "=" + suffix
	otherFixtureLabel := labelKey + "=other-" + suffix
	managedLabelArg := managedLabel + "=true"

	createdName := "containergo-prune-created-" + suffix
	exitedName := "containergo-prune-exited-" + suffix
	runningName := "containergo-prune-running-" + suffix
	unmanagedName := "containergo-prune-unmanaged-" + suffix
	otherManagedName := "containergo-prune-other-managed-" + suffix
	names := []string{createdName, exitedName, runningName, unmanagedName, otherManagedName}
	t.Cleanup(func() {
		for _, name := range names {
			_ = exec.Command("docker", "rm", "--force", name).Run()
		}
	})

	runDockerPruneCommand(t, "create", "--label", managedLabelArg, "--label", fixtureLabel, "--name", createdName, integrationAlpine)
	runDockerPruneCommand(t, "create", "--label", managedLabelArg, "--label", fixtureLabel, "--name", exitedName, integrationAlpine, "true")
	runDockerPruneCommand(t, "create", "--label", fixtureLabel, "--name", unmanagedName, integrationAlpine, "true")
	runDockerPruneCommand(t, "create", "--label", managedLabelArg, "--label", otherFixtureLabel, "--name", otherManagedName, integrationAlpine, "true")
	runDockerPruneCommand(t, "start", exitedName, unmanagedName, otherManagedName)
	// Keep this fixture alive for the whole test; a finite sleep can expire
	// while Docker is busy and turn the preservation assertion into a race.
	runDockerPruneCommand(t, "run", "--detach", "--label", managedLabelArg, "--label", fixtureLabel, "--name", runningName, integrationAlpine, "sh", "-c", "while :; do sleep 3600; done")

	waitDockerContainerState(t, createdName, "created")
	waitDockerContainerState(t, exitedName, "exited")
	waitDockerContainerState(t, runningName, "running")
	waitDockerContainerState(t, unmanagedName, "exited")
	waitDockerContainerState(t, otherManagedName, "exited")

	exitedID := runDockerPruneCommand(t, "inspect", "--format", "{{.Id}}", exitedName)
	exitedListID := runDockerPruneCommand(t, "ps", "--all", "--quiet", "--filter", "name=^/"+exitedName+"$")
	if exitedListID == "" || strings.Contains(exitedListID, "\n") {
		t.Fatalf("could not determine exact Docker list ID for %s: %q", exitedName, exitedListID)
	}
	otherManagedID := runDockerPruneCommand(t, "inspect", "--format", "{{.Id}}", otherManagedName)
	unmanagedID := runDockerPruneCommand(t, "inspect", "--format", "{{.Id}}", unmanagedName)
	createdID := runDockerPruneCommand(t, "inspect", "--format", "{{.Id}}", createdName)
	runningID := runDockerPruneCommand(t, "inspect", "--format", "{{.Id}}", runningName)

	runner := &isolatedDockerPruneRunner{
		runner:      &cli.ExecRunner{Binary: "docker"},
		labelFilter: "label=" + fixtureLabel,
	}
	removed, err := pruneWith(ctx, runner, dockerEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(runner.calls) == 0 || runner.calls[0][0] != "ps" {
		t.Fatalf("Prune calls = %v, want a docker ps call", runner.calls)
	}
	if !slices.Contains(runner.calls[0], "label="+fixtureLabel) {
		t.Fatalf("Prune list call lacks fixture scope: %v", runner.calls[0])
	}
	for _, want := range []string{"status=exited", "status=dead"} {
		if !slices.Contains(runner.calls[0], want) {
			t.Fatalf("Prune list call lacks %q: %v", want, runner.calls[0])
		}
	}
	for _, unwanted := range []string{"status=created", "status=running"} {
		if slices.Contains(runner.calls[0], unwanted) {
			t.Fatalf("Prune list call unexpectedly includes %q: %v", unwanted, runner.calls[0])
		}
	}
	if len(removed) != 1 || removed[0] != exitedListID || !strings.HasPrefix(exitedID, removed[0]) {
		t.Fatalf("removed = %v, want exact Docker list ID %s (full ID %s)", removed, exitedListID, exitedID)
	}
	unrelated := []struct{ role, id string }{
		{"created", createdID},
		{"running", runningID},
		{"unmanaged", unmanagedID},
		{"other managed", otherManagedID},
	}
	for _, u := range unrelated {
		if removed[0] == u.id || strings.HasPrefix(u.id, removed[0]) {
			t.Errorf("Prune returned unrelated %s container %s", u.role, u.id)
		}
	}

	if state, found, err := inspectDockerContainerState(exitedName); err != nil {
		t.Fatalf("inspect removed exited fixture: %v", err)
	} else if found {
		t.Fatalf("exited container %s survived Prune (state=%q)", exitedName, state)
	}
	preserved := []struct{ name, want string }{
		{createdName, "created"},
		{runningName, "running"},
		{unmanagedName, "exited"},
		{otherManagedName, "exited"},
	}
	for _, p := range preserved {
		state, found, err := inspectDockerContainerState(p.name)
		if err != nil {
			t.Errorf("inspect preserved fixture %s: %v", p.name, err)
			continue
		}
		if !found {
			t.Errorf("fixture %s was removed by Prune", p.name)
			continue
		}
		if state != p.want {
			t.Errorf("state of %s = %q, want %q", p.name, state, p.want)
		}
	}
}

// TestIntegrationDockerPruneDeadStatusFilterAvailability verifies the live
// daemon accepts the dead status filter. Docker has no stable public API for
// forcing a container into dead state, so an empty result is an explicit
// limitation rather than a fabricated dead fixture.
func TestIntegrationDockerPruneDeadStatusFilterAvailability(t *testing.T) {
	requireDockerPruneIntegration(t)
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label="+managedLabel+"=true", "--filter", "status=dead").CombinedOutput()
	if err != nil {
		t.Fatalf("docker rejected status=dead: %v: %s", err, out)
	}

	ids := splitNonEmptyLines(out)
	if len(ids) == 0 {
		t.Skip("docker accepts status=dead, but no dead managed container exists; Docker has no public API to create one")
	}
	for _, id := range ids {
		state, found, err := inspectDockerContainerState(id)
		if err != nil {
			t.Fatalf("inspect dead container %s: %v", id, err)
		}
		if !found {
			t.Fatalf("dead container %s disappeared during inspection", id)
		}
		if state != "dead" {
			t.Fatalf("status=dead returned %s with state %q", id, state)
		}
	}
}
