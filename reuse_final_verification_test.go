package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type issue83FinalVerifySpec struct {
	name     string
	uid      string
	creation string
	state    string
	image    string
	platform string
	ports    []boundPort
}

type issue83FinalVerifyBackend struct {
	name string
	eng  engine
	uid  string
	json func(issue83FinalVerifySpec) []byte
}

func issue83FinalVerifyBackends() []issue83FinalVerifyBackend {
	return []issue83FinalVerifyBackend{
		{
			name: "docker",
			eng:  dockerEngine{},
			uid:  strings.Repeat("a", 64),
			json: issue83FinalDockerInspect,
		},
		{
			name: "apple",
			eng:  appleEngine{},
			json: issue83FinalAppleInspect,
		},
	}
}

func (b issue83FinalVerifyBackend) spec() issue83FinalVerifySpec {
	platform := "linux/arm64"
	if b.eng.name() == "docker" {
		// Docker container inspect reports Platform as the OS only.
		platform = "linux"
	}
	return issue83FinalVerifySpec{
		name:     "final-" + b.name,
		uid:      b.uid,
		creation: "aaaaaaaaaaaaaaaa",
		state:    "running",
		image:    "redis:7-alpine",
		platform: platform,
		ports: []boundPort{{
			containerPort: 6379,
			proto:         "tcp",
			hostAddr:      "127.0.0.1",
			hostPort:      49153,
		}},
	}
}

func issue83FinalLabels(creation string) map[string]string {
	return map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
}

func issue83FinalDockerInspect(spec issue83FinalVerifySpec) []byte {
	ports := make(map[string][]map[string]string, len(spec.ports))
	for _, port := range spec.ports {
		key := fmt.Sprintf("%d/%s", port.containerPort, port.proto)
		ports[key] = []map[string]string{{
			"HostIp":   port.hostAddr,
			"HostPort": fmt.Sprint(port.hostPort),
		}}
	}
	data, _ := json.Marshal([]map[string]any{{
		"Id":       spec.uid,
		"Name":     "/" + spec.name,
		"State":    map[string]string{"Status": spec.state},
		"Config":   map[string]any{"Image": spec.image, "Labels": issue83FinalLabels(spec.creation)},
		"Platform": spec.platform,
		"NetworkSettings": map[string]any{
			"IPAddress": "172.17.0.2",
			"Ports":     ports,
		},
	}})
	return data
}

func issue83FinalAppleInspect(spec issue83FinalVerifySpec) []byte {
	ports := make([]map[string]any, 0, len(spec.ports))
	for _, port := range spec.ports {
		ports = append(ports, map[string]any{
			"hostAddress":   port.hostAddr,
			"hostPort":      port.hostPort,
			"containerPort": port.containerPort,
			"proto":         port.proto,
		})
	}
	platform := strings.Split(spec.platform, "/")
	platformJSON := map[string]string{"os": platform[0]}
	if len(platform) > 1 {
		platformJSON["architecture"] = platform[1]
	}
	data, _ := json.Marshal([]map[string]any{{
		"id": spec.name,
		"configuration": map[string]any{
			"id":             spec.name,
			"image":          map[string]string{"reference": spec.image},
			"labels":         issue83FinalLabels(spec.creation),
			"platform":       platformJSON,
			"publishedPorts": ports,
		},
		"status": map[string]any{
			"state": spec.state,
			"networks": []map[string]string{{
				"ipv4Address": "192.168.64.3/24",
				"network":     "default",
			}},
		},
	}})
	return data
}

type issue83FinalVerifyRunner struct {
	*fakeRunner
	before []byte
	after  []byte
	mu     sync.Mutex
	calls  int
}

func (r *issue83FinalVerifyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.calls++
	n := r.calls
	after := r.after
	before := r.before
	r.mu.Unlock()
	if n > 1 {
		return after, nil, nil
	}
	return before, nil, nil
}

type issue83FinalVerifyStrategy struct{}

func (issue83FinalVerifyStrategy) WaitUntilReady(context.Context, wait.Target) error {
	return nil
}

func TestIssue83ReuseFinalVerificationRejectsBackendReplacement(t *testing.T) {
	for _, backend := range issue83FinalVerifyBackends() {
		t.Run(backend.name, func(t *testing.T) {
			before := backend.spec()
			after := before
			if backend.eng.name() == "docker" {
				after.uid = strings.Repeat("b", 64)
			} else {
				after.creation = "bbbbbbbbbbbbbbbb"
			}
			runner := &issue83FinalVerifyRunner{
				fakeRunner: newTestRunner(),
				before:     backend.json(before),
				after:      backend.json(after),
			}
			runner.imagePresent = true
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(before.name), WithReuse(), WithPlatform("linux/arm64"), WithPublishedPort("127.0.0.1:49153:6379"),
				WithWaitStrategy(issue83FinalVerifyStrategy{}), withRunner(runner), withEngine(backend.eng))
			if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("Run = (%v, %v), want replacement error", ctr, err)
			}
			if runner.calls != 2 {
				t.Fatalf("inspect calls = %d, want initial and final", runner.calls)
			}
		})
	}
}

func TestIssue83ReuseChecksRequestedPlatform(t *testing.T) {
	backends := issue83FinalVerifyBackends()
	tests := []struct {
		name     string
		backend  int
		platform string
		wantErr  bool
	}{
		{
			name:     "docker OS-only inspect accepts architecture selector",
			backend:  0,
			platform: "linux/arm64",
		},
		{
			name:     "docker rejects OS mismatch",
			backend:  0,
			platform: "windows/arm64",
			wantErr:  true,
		},
		{
			name:     "apple OS selector is unconstrained",
			backend:  1,
			platform: "linux",
		},
		{
			name:     "apple rejects architecture mismatch",
			backend:  1,
			platform: "linux/amd64",
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := backends[tc.backend]
			spec := backend.spec()
			runner := &issue83FinalVerifyRunner{
				fakeRunner: newTestRunner(),
				before:     backend.json(spec),
				after:      backend.json(spec),
			}
			runner.imagePresent = true
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(spec.name), WithReuse(), WithPlatform(tc.platform),
				withRunner(runner), withEngine(backend.eng))
			if tc.wantErr {
				if err == nil || ctr != nil || !strings.Contains(err.Error(), "platform") {
					t.Fatalf("Run = (%v, %v), want platform mismatch", ctr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr == nil || ctr.creation != spec.creation {
				t.Fatalf("handle = %+v, want creation %q", ctr, spec.creation)
			}
		})
	}
}

func TestIssue83ReuseRejectsDockerGenerationChangeWithSameUID(t *testing.T) {
	backend := issue83FinalVerifyBackends()[0]
	before := backend.spec()
	after := before
	after.creation = "bbbbbbbbbbbbbbbb"
	runner := &issue83FinalVerifyRunner{
		fakeRunner: newTestRunner(),
		before:     backend.json(before),
		after:      backend.json(after),
	}
	runner.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(before.name), WithReuse(), WithPlatform("linux/arm64"), WithPublishedPort("127.0.0.1:49153:6379"),
		WithWaitStrategy(issue83FinalVerifyStrategy{}), withRunner(runner), withEngine(backend.eng))
	if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Run = (%v, %v), want generation replacement error", ctr, err)
	}
}

func TestIssue83ReuseFinalVerificationFailsClosedOnMissingGeneration(t *testing.T) {
	backend := issue83FinalVerifyBackends()[0]
	before := backend.spec()
	after := before
	after.creation = ""
	runner := &issue83FinalVerifyRunner{
		fakeRunner: newTestRunner(),
		before:     backend.json(before),
		after:      backend.json(after),
	}
	runner.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(before.name), WithReuse(), WithPlatform("linux/arm64"), WithPublishedPort("127.0.0.1:49153:6379"),
		WithWaitStrategy(issue83FinalVerifyStrategy{}), withRunner(runner), withEngine(backend.eng))
	if err == nil || ctr != nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("Run = (%v, %v), want missing-generation refusal", ctr, err)
	}
}

func TestIssue83ReuseFinalVerificationRejectsChangedReadinessResult(t *testing.T) {
	for _, backend := range issue83FinalVerifyBackends() {
		t.Run(backend.name, func(t *testing.T) {
			before := backend.spec()
			after := before
			after.state = "stopped"
			runner := &issue83FinalVerifyRunner{
				fakeRunner: newTestRunner(),
				before:     backend.json(before),
				after:      backend.json(after),
			}
			runner.imagePresent = true
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(before.name), WithReuse(), WithPlatform("linux/arm64"), WithPublishedPort("127.0.0.1:49153:6379"),
				WithWaitStrategy(issue83FinalVerifyStrategy{}), withRunner(runner), withEngine(backend.eng))
			if err == nil || ctr != nil || !strings.Contains(err.Error(), "state") {
				t.Fatalf("Run = (%v, %v), want stopped-state refusal", ctr, err)
			}
		})
	}
}

func TestIssue83ReuseFinalVerificationAcceptsUnchangedResult(t *testing.T) {
	for _, backend := range issue83FinalVerifyBackends() {
		t.Run(backend.name, func(t *testing.T) {
			spec := backend.spec()
			runner := &issue83FinalVerifyRunner{
				fakeRunner: newTestRunner(),
				before:     backend.json(spec),
				after:      backend.json(spec),
			}
			runner.imagePresent = true
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(spec.name), WithReuse(), WithPlatform("linux/arm64"), WithPublishedPort("127.0.0.1:49153:6379"),
				WithWaitStrategy(issue83FinalVerifyStrategy{}), withRunner(runner), withEngine(backend.eng))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr == nil || ctr.creation != spec.creation {
				t.Fatalf("handle = %+v, want creation %q", ctr, spec.creation)
			}
			if backend.eng.name() == "docker" && ctr.uid != spec.uid {
				t.Fatalf("uid = %q, want %q", ctr.uid, spec.uid)
			}
		})
	}
}
