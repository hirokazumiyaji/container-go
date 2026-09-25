package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type reuseInspectSpec struct {
	uid      string
	state    string
	image    string
	creation string
	platform string
	ports    []boundPort
}

type reuseBackendFixture struct {
	name     string
	eng      engine
	uid      string
	creation string
	inspect  func(reuseInspectSpec) []byte
}

func reuseBackendFixtures() []reuseBackendFixture {
	return []reuseBackendFixture{
		{
			name:     "docker",
			eng:      dockerEngine{},
			uid:      strings.Repeat("a", 64),
			creation: "aaaaaaaaaaaaaaaa",
			inspect:  dockerReuseInspectJSON,
		},
		{
			name:     "apple",
			eng:      appleEngine{},
			creation: "aaaaaaaaaaaaaaaa",
			inspect:  appleReuseInspectJSON,
		},
	}
}

func (f reuseBackendFixture) runningSpec() reuseInspectSpec {
	return reuseInspectSpec{
		uid:      f.uid,
		state:    "running",
		image:    "redis:7-alpine",
		creation: f.creation,
		platform: "linux/amd64",
		ports: []boundPort{{
			containerPort: 6379,
			proto:         "tcp",
			hostAddr:      "127.0.0.1",
			hostPort:      49153,
		}},
	}
}

func dockerReuseInspectJSON(spec reuseInspectSpec) []byte {
	ports := make(map[string]any, len(spec.ports))
	for _, port := range spec.ports {
		key := fmt.Sprintf("%d/%s", port.containerPort, port.proto)
		ports[key] = []map[string]string{{
			"HostIp":   port.hostAddr,
			"HostPort": fmt.Sprint(port.hostPort),
		}}
	}
	return marshalReuseInspectJSON([]map[string]any{{
		"Id":      spec.uid,
		"Created": "2026-08-19T01:23:45.678901234Z",
		"Image":   "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"Name":    "/shared",
		"State":   map[string]string{"Status": spec.state},
		"Config": map[string]any{
			"Image":  spec.image + "@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"Labels": reuseInspectLabels(spec.creation),
		},
		"Platform": spec.platform,
		"NetworkSettings": map[string]any{
			"IPAddress": "172.17.0.2",
			"Ports":     ports,
		},
	}})
}

func appleReuseInspectJSON(spec reuseInspectSpec) []byte {
	ports := make([]map[string]any, 0, len(spec.ports))
	for _, port := range spec.ports {
		ports = append(ports, map[string]any{
			"hostAddress":   port.hostAddr,
			"hostPort":      port.hostPort,
			"containerPort": port.containerPort,
			"proto":         port.proto,
		})
	}
	return marshalReuseInspectJSON([]map[string]any{{
		"id": "shared",
		"configuration": map[string]any{
			"id": "shared",
			"image": map[string]any{
				"reference":  spec.image,
				"descriptor": map[string]string{"digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			},
			"labels":         reuseInspectLabels(spec.creation),
			"platform":       platformInspectJSON(spec.platform),
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
}

func platformInspectJSON(platform string) map[string]string {
	parts := strings.Split(platform, "/")
	out := map[string]string{"os": parts[0]}
	if len(parts) > 1 {
		out["architecture"] = parts[1]
	}
	if len(parts) > 2 {
		out["variant"] = parts[2]
	}
	return out
}

func reuseInspectLabels(creation string) map[string]string {
	return map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
}

func marshalReuseInspectJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

type reuseTransitionRunner struct {
	*fakeRunner
	before            []byte
	after             []byte
	currentIsAfter    bool
	replaceAfterFirst bool
	inspectCalls      int
}

func newReuseTransitionRunner(before, after []byte) *reuseTransitionRunner {
	return &reuseTransitionRunner{
		fakeRunner: newTestRunner(),
		before:     before,
		after:      after,
	}
}

func (r *reuseTransitionRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	r.inspectCalls++
	if r.replaceAfterFirst && r.inspectCalls > 1 {
		r.currentIsAfter = true
	}
	if r.currentIsAfter {
		return r.after, nil, nil
	}
	return r.before, nil, nil
}

func (r *reuseTransitionRunner) replace() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.currentIsAfter = true
}

type replaceDuringWaitStrategy struct {
	replace func()
	called  bool
}

func (s *replaceDuringWaitStrategy) WaitUntilReady(_ context.Context, _ wait.Target) error {
	s.called = true
	s.replace()
	return nil
}

func TestReuseRejectsReplacementDuringWait(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			before := backend.runningSpec()
			after := before
			if backend.eng.name() == "docker" {
				after.uid = strings.Repeat("b", 64)
			} else {
				after.creation = "bbbbbbbbbbbbbbbb"
			}
			runner := newReuseTransitionRunner(backend.inspect(before), backend.inspect(after))
			strategy := &replaceDuringWaitStrategy{replace: runner.replace}

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(),
				withRunner(runner), withEngine(backend.eng),
				WithWaitStrategy(strategy),
			)
			if err == nil || ctr != nil {
				t.Fatalf("Run = (%v, %v), want nil handle and replacement error", ctr, err)
			}
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("error = %v, want ErrGenerationReplaced", err)
			}
			if !strategy.called {
				t.Fatal("wait strategy was not called")
			}
			if runner.inspectCalls != 2 {
				t.Fatalf("inspect calls = %d, want initial and post-wait inspect", runner.inspectCalls)
			}
		})
	}
}

func TestReuseVerifiesGenerationWithoutWait(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			before := backend.runningSpec()
			after := before
			if backend.eng.name() == "docker" {
				after.uid = strings.Repeat("b", 64)
			} else {
				after.creation = "bbbbbbbbbbbbbbbb"
			}
			runner := newReuseTransitionRunner(backend.inspect(before), backend.inspect(after))
			runner.replaceAfterFirst = true

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(),
				withRunner(runner), withEngine(backend.eng),
			)
			if err == nil || ctr != nil {
				t.Fatalf("Run = (%v, %v), want nil handle and replacement error", ctr, err)
			}
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("error = %v, want ErrGenerationReplaced", err)
			}
			if runner.inspectCalls != 2 {
				t.Fatalf("inspect calls = %d, want initial and final inspect", runner.inspectCalls)
			}
		})
	}
}

func TestReuseSucceedsWhenGenerationUnchangedAfterWait(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			spec := backend.runningSpec()
			runner := newReuseTransitionRunner(backend.inspect(spec), backend.inspect(spec))
			strategy := &replaceDuringWaitStrategy{replace: func() {}}

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(),
				withRunner(runner), withEngine(backend.eng),
				WithWaitStrategy(strategy),
			)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr == nil || ctr.creation != spec.creation {
				t.Fatalf("handle = %+v, want creation %q", ctr, spec.creation)
			}
			if backend.eng.name() == "docker" && ctr.uid != spec.uid {
				t.Fatalf("uid = %q, want %q", ctr.uid, spec.uid)
			}
			if runner.inspectCalls != 2 {
				t.Fatalf("inspect calls = %d, want initial and post-wait inspect", runner.inspectCalls)
			}
		})
	}
}

func TestReuseRejectsChangedContainerDuringWait(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	mutations := []struct {
		name   string
		change func(*reuseInspectSpec)
		want   string
	}{
		{
			name: "image",
			change: func(spec *reuseInspectSpec) {
				spec.image = "nginx:alpine"
			},
			want: "image",
		},
		{
			name: "port",
			change: func(spec *reuseInspectSpec) {
				spec.ports[0].hostPort++
			},
			want: "port",
		},
		{
			name: "platform",
			change: func(spec *reuseInspectSpec) {
				spec.platform = "linux/arm64"
			},
			want: "platform",
		},
		{
			name: "state",
			change: func(spec *reuseInspectSpec) {
				spec.state = "stopped"
			},
			want: "state",
		},
	}

	for _, backend := range reuseBackendFixtures() {
		for _, mutation := range mutations {
			t.Run(backend.name+"/"+mutation.name, func(t *testing.T) {
				before := backend.runningSpec()
				after := before
				after.ports = append([]boundPort(nil), before.ports...)
				mutation.change(&after)
				runner := newReuseTransitionRunner(backend.inspect(before), backend.inspect(after))
				strategy := &replaceDuringWaitStrategy{replace: runner.replace}

				ctr, err := Run(context.Background(), "redis:7-alpine",
					WithName("shared"), WithReuse(),
					withRunner(runner), withEngine(backend.eng),
					WithPublishedPort("127.0.0.1:49153:6379"),
					WithPlatform("linux/amd64"),
					WithWaitStrategy(strategy),
				)
				if err == nil || ctr != nil {
					t.Fatalf("Run = (%v, %v), want changed-%s error", ctr, err, mutation.name)
				}
				if !strings.Contains(err.Error(), mutation.want) {
					t.Fatalf("error = %v, want mention of changed %s", err, mutation.want)
				}
			})
		}
	}
}
