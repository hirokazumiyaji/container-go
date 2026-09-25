package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestCleanupFailedCreateRejectsUnexpectedReuseMarkers(t *testing.T) {
	const creation = "0123456789abcdef"
	runErr := errors.New("run failed")
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{
			name: "reuse marker",
			labels: map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: creation,
				reuseLabel:    "true",
			},
		},
		{
			name: "reuse group marker",
			labels: map[string]string{
				managedLabel:    "true",
				sessionLabel:    sessionID(),
				creationLabel:   creation,
				reuseGroupLabel: "integration",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &failRunRunner{
				fakeRunner:  newTestRunner(),
				inspectJSON: inspectJSONWithStateAndLabels("shared-cleanup", "running", "redis:7-alpine", tc.labels),
			}
			cfg := &config{
				runner:   runner,
				eng:      appleEngine{},
				name:     "shared-cleanup",
				creation: creation,
			}
			err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("cleanupFailedCreate error = %v, want rejection of %s", err, tc.name)
			}
			if len(runner.deleted) != 0 {
				t.Fatalf("deleted shared generation: %v", runner.deleted)
			}
		})
	}
}

type reviewOwnershipRunner struct {
	inspectJSON string
	inspects    []string
	deletes     []string
}

func (r *reviewOwnershipRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.inspects = append(r.inspects, args[len(args)-1])
		return []byte(r.inspectJSON), nil, nil
	case "delete", "rm":
		r.deletes = append(r.deletes, args[len(args)-1])
	}
	return nil, nil, nil
}

func reviewDockerOwnershipInspect(uid, name, state string, labels map[string]string) string {
	data, _ := json.Marshal([]map[string]any{{
		"Id":    uid,
		"Name":  "/" + name,
		"State": map[string]string{"Status": state},
		"Config": map[string]any{
			"Image":  "redis:7-alpine",
			"Labels": labels,
		},
	}})
	return string(data)
}

func TestTerminateContainerRejectsUnexpectedReuseMarkers(t *testing.T) {
	const creation = "0123456789abcdef"
	baseLabels := map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: creation,
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "reuse marker", labels: withReviewLabel(baseLabels, reuseLabel, "true")},
		{name: "group marker", labels: withReviewLabel(baseLabels, reuseGroupLabel, "integration")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &reviewOwnershipRunner{
				inspectJSON: inspectJSONWithStateAndLabels(
					"apple-shared", "running", "redis:7-alpine", tc.labels,
				),
			}
			ctr := &Container{
				id: "apple-shared", runner: runner, eng: appleEngine{}, creation: creation,
			}
			if err := TerminateContainer(ctr); err == nil {
				t.Fatal("TerminateContainer accepted a shared generation")
			}
			if len(runner.deletes) != 0 {
				t.Fatalf("deleted shared generation: %v", runner.deletes)
			}
			if len(runner.inspects) == 0 {
				t.Fatal("shared markers were not checked with a fresh inspect")
			}
		})
	}
}

func withReviewLabel(labels map[string]string, key, value string) map[string]string {
	copy := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		copy[k] = v
	}
	copy[key] = value
	return copy
}

func TestRollbackPreservesUnexpectedReuseGeneration(t *testing.T) {
	const creation = "0123456789abcdef"
	labels := map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: creation,
		reuseLabel:    "true",
	}
	runner := &reviewOwnershipRunner{
		inspectJSON: inspectJSONWithStateAndLabels("rollback-shared", "running", "redis:7-alpine", labels),
	}
	ctr := &Container{id: "rollback-shared", runner: runner, eng: appleEngine{}, creation: creation}
	cause := errors.New("copy failed")
	err := ctr.rollback(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("rollback error = %v, want primary cause", err)
	}
	if len(runner.deletes) != 0 {
		t.Fatalf("rollback deleted shared generation: %v", runner.deletes)
	}
}

func TestCreatedOwnershipRejectsUnexpectedReuseMarkersBeforeReaperRegistration(t *testing.T) {
	const (
		creation = "0123456789abcdef"
		uid      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	for _, marker := range []string{reuseLabel, reuseGroupLabel} {
		t.Run(marker, func(t *testing.T) {
			labels := map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: creation,
				marker:        "unexpected",
			}
			for _, eng := range []engine{appleEngine{}, dockerEngine{}} {
				runner := &reviewOwnershipRunner{}
				name := "reaper-owned"
				if eng.name() == "docker" {
					name = uid
					runner.inspectJSON = reviewDockerOwnershipInspect(uid, "reaper-owned", "running", labels)
				} else {
					runner.inspectJSON = inspectJSONWithStateAndLabels(name, "running", "redis:7-alpine", labels)
				}
				ctr := &Container{
					id: "reaper-owned", runner: runner, eng: eng,
					creation: creation, uid: map[bool]string{true: uid, false: ""}[eng.name() == "docker"],
				}
				cfg := &config{eng: eng, name: "reaper-owned", creation: creation}
				if err := verifyCreatedOwnership(context.Background(), ctr, cfg); err == nil {
					t.Fatalf("%s accepted unexpected %s before reaper registration", eng.name(), marker)
				}
			}
		})
	}
}

func TestOrdinaryPruneRejectsSharedGenerationMarkers(t *testing.T) {
	base := pruneCandidate{
		id: "ordinary", managed: true, creation: "0123456789abcdef", state: StateStopped,
		labels: map[string]string{managedLabel: "true"},
	}
	if !pruneCandidateEligible(base, "") {
		t.Fatal("ordinary stopped generation was rejected")
	}
	for _, marker := range []string{reuseLabel, reuseGroupLabel} {
		shared := base
		shared.labels = withReviewLabel(base.labels, marker, "unexpected")
		shared.reuse = marker == reuseLabel && shared.labels[marker] == "true"
		shared.reuseGroup = shared.labels[reuseGroupLabel]
		if pruneCandidateEligible(shared, "") {
			t.Fatalf("ordinary prune accepted %s", marker)
		}
	}

	dockerBase := dockerPruneCandidate{
		listedID: strings.Repeat("a", 64), uid: strings.Repeat("a", 64),
		managed: true, creation: "0123456789abcdef", state: StateStopped,
		labels: map[string]string{managedLabel: "true"},
	}
	if !dockerPruneCandidateEligible(dockerBase, "") {
		t.Fatal("ordinary stopped Docker generation was rejected")
	}
	for _, marker := range []string{reuseLabel, reuseGroupLabel} {
		shared := dockerBase
		shared.labels = withReviewLabel(dockerBase.labels, marker, "unexpected")
		shared.reuse = shared.labels[reuseLabel] == "true"
		shared.reuseGroup = shared.labels[reuseGroupLabel]
		if dockerPruneCandidateEligible(shared, "") {
			t.Fatalf("ordinary Docker prune accepted %s", marker)
		}
	}
}

type freshReuseGroupRunner struct {
	*fakeRunner
	created     atomic.Bool
	creation    atomic.Value
	actualGroup string
}

func (r *freshReuseGroupRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation.Store(creation)
				}
			}
		}
		r.created.Store(true)
	}
	if args[0] == "inspect" {
		if !r.created.Load() {
			return nil, nil, &cli.CLIError{
				Binary: "docker", Args: args, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: " + args[len(args)-1],
			}
		}
		creation, _ := r.creation.Load().(string)
		group := ""
		if r.actualGroup != "" {
			group = fmt.Sprintf(`,"%s":%q`, reuseGroupLabel, r.actualGroup)
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/group-target","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{"%s":"true","%s":"true","%s":%q%s}}}]`,
			strings.Repeat("a", 64), managedLabel, reuseLabel, creationLabel, creation, group)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestFreshReuseRequiresExactReuseGroupLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		actual    string
	}{
		{name: "missing", requested: "integration"},
		{name: "rewritten", requested: "integration", actual: "other"},
		{name: "unexpected without request", actual: "integration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestRunner()
			base.imagePresent = true
			runner := &freshReuseGroupRunner{fakeRunner: base, actualGroup: tc.actual}
			opts := []Option{
				WithName("fresh-group-" + newContainerName()), WithReuse(),
				withRunner(runner), withEngine(dockerEngine{}),
			}
			if tc.requested != "" {
				opts = append(opts, WithReuseGroup(tc.requested))
			}
			ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("Run = (%v, %v), want exact group verification failure", ctr, err)
			}
		})
	}
}

type finalDockerDisappearanceRunner struct {
	*fakeRunner
	inspectCalls atomic.Int32
	creation     atomic.Value
}

func (r *finalDockerDisappearanceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation.Store(creation)
				}
			}
		}
	}
	if args[0] == "inspect" {
		n := r.inspectCalls.Add(1)
		switch n {
		case 1:
			return nil, nil, &cli.CLIError{
				Binary: "docker", Args: args, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: " + args[len(args)-1],
			}
		case 2:
			creation, _ := r.creation.Load().(string)
			uid := strings.Repeat("a", 64)
			return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/disappearing","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{"%s":"true","%s":"true","%s":%q}}}]`,
				uid, managedLabel, reuseLabel, creationLabel, creation)), nil, nil
		default:
			return nil, nil, &cli.CLIError{
				Binary: "docker", Args: args, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: " + args[len(args)-1],
			}
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestHostWithPublishedAddressDoesNotRequireLiveInspect(t *testing.T) {
	runner := newTestRunner()
	ctr := runTestContainer(t, runner,
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("127.0.0.1:16379:6379/tcp"),
	)
	runner.failPrefix = "inspect"
	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host from configured publish address: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("Host = %q, want configured address", host)
	}
}

func TestDirectAppleHostStillUsesFreshIdentity(t *testing.T) {
	const (
		name     = "fresh-host"
		creation = "aaaaaaaaaaaaaaaa"
	)
	runner := &dynamicEndpointRunner{
		fakeRunner: newTestRunner(),
		responses: []string{
			dynamicAppleEndpointInspect(name, creation, "192.168.64.3"),
		},
	}
	ctr := &Container{
		id: name, runner: runner, eng: appleEngine{}, creation: creation,
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}
	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "192.168.64.3" {
		t.Fatalf("Host = %q, want fresh direct IP", host)
	}
	if runner.inspects != 1 {
		t.Fatalf("inspect calls = %d, want fresh direct-IP inspect", runner.inspects)
	}
}

func TestFinalDockerDisappearancePreservesJoinedCauses(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &finalDockerDisappearanceRunner{fakeRunner: base}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("disappearing"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	if ctr != nil || err == nil {
		t.Fatalf("Run = (%v, %v), want final disappearance error", ctr, err)
	}
	if !errors.Is(err, ErrGenerationReplaced) || !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want joined generation-replaced and not-found causes", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want CLIError details", err)
	}
	if !strings.Contains(cliErr.Stderr, "No such container") {
		t.Fatalf("CLIError stderr = %q, want Docker not-found details", cliErr.Stderr)
	}
}
