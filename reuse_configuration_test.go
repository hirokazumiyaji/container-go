package container

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

var defaultReuseFingerprint = newConfig().reuseConfigHash()

func TestReuseConfigurationCompatibility(t *testing.T) {
	options := map[string]Option{
		"memory": WithMemory("512M"), "user": WithUser("1000"),
		"workdir": WithWorkingDir("/app"), "entrypoint": WithEntrypoint("/bin/app"),
		"cmd":    WithCmd("serve", "--quiet"),
		"mount":  WithMounts(Mount{Type: MountVolume, Source: "data", Target: "/data"}),
		"labels": WithLabels(map[string]string{"team": "backend"}),
	}
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			for name, option := range options {
				t.Run(name, func(t *testing.T) {
					cfg := newConfig()
					cfg.reuse = true
					if err := option(cfg); err != nil {
						t.Fatal(err)
					}
					var entries []map[string]any
					if err := json.Unmarshal(backend.inspect(backend.runningSpec()), &entries); err != nil {
						t.Fatal(err)
					}
					section, labelKey := "Config", "Labels"
					if backend.name == "apple" {
						section, labelKey = "configuration", "labels"
					}
					labels := entries[0][section].(map[string]any)[labelKey].(map[string]any)
					labels[domain.LabelReuseConfig] = cfg.allLabels()[domain.LabelReuseConfig]
					data, err := json.Marshal(entries)
					if err != nil {
						t.Fatal(err)
					}
					for _, matching := range []bool{true, false} {
						runner := newReuseTransitionRunner(data, data, backend.eng)
						opts := []Option{WithName("shared"), WithReuse(), withRunner(runner), withEngine(backend.eng)}
						if matching {
							opts = append(opts, option)
						}
						ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
						if matching {
							if err != nil {
								t.Fatalf("matching configuration: %v", err)
							}
							if ctr == nil {
								t.Fatal("missing reused handle")
							}
						} else if !errors.Is(err, ErrReuseMismatch) {
							t.Fatalf("removed option: got %v, want ErrReuseMismatch", err)
						}
					}
					delete(labels, domain.LabelReuseConfig)
					data, err = json.Marshal(entries)
					if err != nil {
						t.Fatal(err)
					}
					runner := newReuseTransitionRunner(data, data, backend.eng)
					_, err = Run(context.Background(), "redis:7-alpine", WithName("shared"), WithReuse(), withRunner(runner), withEngine(backend.eng), option)
					if !errors.Is(err, ErrReuseMismatch) {
						t.Fatalf("unverifiable configuration: got %v, want ErrReuseMismatch", err)
					}
				})
			}
		})
	}
}

func TestReuseFlightKeyIncludesConfiguration(t *testing.T) {
	base := newConfig()
	base.eng = dockerEngine{}
	base.name = "shared"
	for name, option := range map[string]Option{
		"memory": WithMemory("512M"), "user": WithUser("1000"),
		"workdir": WithWorkingDir("/app"), "entrypoint": WithEntrypoint("/app"),
		"cmd": WithCmd("serve"), "labels": WithLabels(map[string]string{"team": "backend"}),
		"mount":    WithMounts(Mount{Type: MountVolume, Source: "data", Target: "/data"}),
		"platform": WithPlatform("linux/arm64"), "network": WithNetwork("test"),
		"exposed": WithExposedPorts("80"), "published": WithPublishedPort("8080:80"),
		"group": WithReuseGroup("tests"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := newConfig()
			cfg.eng = dockerEngine{}
			cfg.name = "shared"
			if err := option(cfg); err != nil {
				t.Fatal(err)
			}
			if reuseFlightKey(cfg, "image") == reuseFlightKey(base, "image") {
				t.Fatal("different configurations share one flight")
			}
		})
	}
}

func TestReuseFlightKeyIncludesImage(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "shared"
	if reuseFlightKey(cfg, "redis:7") == reuseFlightKey(cfg, "postgres:17") {
		t.Fatal("different images share a flight")
	}
}

func TestReuseConfigurationCanonicalization(t *testing.T) {
	first, second := newConfig(), newConfig()
	first.memory, second.memory = "512M", "536870912"
	first.cmd, second.cmd = []string{"ab", "c"}, []string{"a", "bc"}
	if first.reuseConfigHash() == second.reuseConfigHash() {
		t.Fatal("different argument boundaries share a fingerprint")
	}
	second.cmd = first.cmd
	first.mounts = []Mount{{Type: MountVolume, Source: "data", Target: "/data"}, {Type: MountTmpfs, Target: "/tmp"}}
	second.mounts = []Mount{first.mounts[1], first.mounts[0]}
	first.labels = map[string]string{"team": "api", "purpose": "tests"}
	second.labels = map[string]string{"purpose": "tests", "team": "api"}
	if first.reuseConfigHash() != second.reuseConfigHash() {
		t.Fatal("equivalent configurations have different fingerprints")
	}
	if err := WithLabels(map[string]string{domain.LabelReuseConfig: "forged"})(first); err == nil {
		t.Fatal("accepted reserved fingerprint label")
	}
}

func TestReuseGroupMustMatch(t *testing.T) {
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.name = "shared"
	cfg.reuseGroup = "requested"
	info, err := cfg.eng.parseInspect(appleReuseInspectJSON(reuseBackendFixtures()[1].runningSpec()), "shared")
	if err != nil {
		t.Fatal(err)
	}
	info.labels[reuseGroupLabel] = "different"
	if err := checkReuseCompat(info, "redis:7-alpine", cfg); !errors.Is(err, ErrReuseMismatch) {
		t.Fatalf("got %v, want group mismatch", err)
	}
}

func TestReuseFingerprintPreservesInvalidUTF8(t *testing.T) {
	first, second := newConfig(), newConfig()
	first.cmd, second.cmd = []string{"\xff"}, []string{"\xfe"}
	if first.reuseConfigHash() == second.reuseConfigHash() {
		t.Fatal("distinct command bytes collide")
	}
	first.cmd, second.cmd = nil, nil
	first.labels, second.labels = map[string]string{"x": "\xff"}, map[string]string{"x": "\xfe"}
	if first.reuseConfigHash() == second.reuseConfigHash() {
		t.Fatal("distinct label bytes collide")
	}
}

func TestReuseFingerprintAlwaysPresentAndReserved(t *testing.T) {
	cfg := newConfig()
	cfg.reuse = true
	if cfg.allLabels()[domain.LabelReuseConfig] == "" {
		t.Fatal("missing default creation fingerprint")
	}
	labels := domain.BuildLabels(domain.LabelConfig{Custom: map[string]string{domain.LabelReuseConfig: "forged"}})
	if labels[domain.LabelReuseConfig] != "" {
		t.Fatal("forged reserved fingerprint survived")
	}
}

func TestReuseMismatchPrecedesPortPolling(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "shared"
	cfg.memory = "512M"
	if err := WithExposedPorts("80")(cfg); err != nil {
		t.Fatal(err)
	}
	spec := reuseBackendFixtures()[0].runningSpec()
	data := dockerReuseInspectJSON(spec)
	runner := newReuseTransitionRunner(data, data, cfg.eng)
	cfg.runner = runner
	base := &Container{id: "shared", runner: runner, eng: cfg.eng, creation: spec.creation, sync: &lockPack{uid: spec.uid}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := reuseInfoForCaller(ctx, cfg, base, imageIdentity{reference: "redis:7-alpine"}, "redis:7-alpine")
	if !errors.Is(err, ErrReuseMismatch) {
		t.Fatalf("got %v, want mismatch before missing port poll", err)
	}
}

func reuseInspectWithLabels(data []byte, labels map[string]string) []byte {
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		panic(err)
	}
	for _, entry := range entries {
		section, key := "Config", "Labels"
		if _, ok := entry["configuration"]; ok {
			section, key = "configuration", "labels"
		}
		config := entry[section].(map[string]any)
		existing := config[key].(map[string]any)
		for k, v := range labels {
			existing[k] = v
		}
	}
	result, err := json.Marshal(entries)
	if err != nil {
		panic(err)
	}
	return result
}

type concurrentReuseConfigRunner struct {
	*fakeRunner
	backend       reuseBackendFixture
	bothInspected chan struct{}
	inspected     int
	created       bool
	creates       int
	labels        map[string]string
}

func (r *concurrentReuseConfigRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		if !r.created {
			r.inspected++
			if r.inspected == 2 {
				close(r.bothInspected)
			}
			r.mu.Unlock()
			message := `Error: notFound: "container not found: shared"`
			if r.backend.name == "docker" {
				message = "Error: No such container: shared"
			}
			return nil, nil, &cli.CLIError{Binary: r.backend.eng.binary(), Args: args, ExitCode: 1, Stderr: message}
		}
		spec := r.backend.runningSpec()
		spec.creation = r.labels[creationLabel]
		data := reuseInspectWithLabels(r.backend.inspect(spec), r.labels)
		r.mu.Unlock()
		return data, nil, nil
	case "run":
		select {
		case <-r.bothInspected:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.creates++
		if r.created {
			message := `Error: container with id shared already exists`
			if r.backend.name == "docker" {
				message = `Conflict. The container name "/shared" is already in use by container`
			}
			return nil, nil, &cli.CLIError{Binary: r.backend.eng.binary(), Args: args, ExitCode: 1, Stderr: message}
		}
		r.labels = map[string]string{}
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				key, value, _ := strings.Cut(args[i+1], "=")
				r.labels[key] = value
			}
		}
		r.created = true
		if r.backend.name == "docker" {
			return []byte(r.backend.uid + "\n"), nil, nil
		}
		return []byte("shared\n"), nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestReuseConcurrentDifferentConfigurations(t *testing.T) {
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			r := &concurrentReuseConfigRunner{fakeRunner: newTestRunner(), backend: backend, bothInspected: make(chan struct{})}
			r.imagePresent = true
			if backend.name == "docker" {
				r.binary = "docker"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results := make(chan error, 2)
			for _, memory := range []string{"512M", "1G"} {
				go func() {
					_, err := Run(ctx, "redis:7-alpine", WithName("shared"), WithReuse(), WithMemory(memory), withRunner(r), withEngine(backend.eng))
					results <- err
				}()
			}
			success, mismatch := 0, 0
			for range 2 {
				err := <-results
				if err == nil {
					success++
				} else if errors.Is(err, ErrReuseMismatch) {
					mismatch++
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if success != 1 || mismatch != 1 || r.creates != 2 {
				t.Fatalf("success=%d mismatch=%d creates=%d, want one success, one mismatch, two racing creates", success, mismatch, r.creates)
			}
		})
	}
}

func TestReuseCompatibilityErrorsUseSentinel(t *testing.T) {
	for name, option := range map[string]Option{
		"cpus":       WithCPUs(2),
		"privileged": WithPrivileged(),
		"network":    WithNetwork("different"),
		"platform":   WithPlatform("linux/s390x"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := newConfig()
			cfg.eng = dockerEngine{}
			cfg.name = "shared"
			if err := option(cfg); err != nil {
				t.Fatal(err)
			}
			backend := reuseBackendFixtures()[0]
			info, err := cfg.eng.parseInspect(backend.inspect(backend.runningSpec()), "shared")
			if err != nil {
				t.Fatal(err)
			}
			if err := checkReuseCompat(info, "redis:7-alpine", cfg); !errors.Is(err, ErrReuseMismatch) {
				t.Fatalf("got %v, want ErrReuseMismatch", err)
			}
		})
	}
}

func TestReuseRunningPortMismatchIsConfigurationError(t *testing.T) {
	backend := reuseBackendFixtures()[0]
	spec := backend.runningSpec()
	data := backend.inspect(spec)
	runner := newReuseTransitionRunner(data, data, backend.eng)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, "redis:7-alpine", WithName("shared"), WithReuse(), WithExposedPorts("9999"), withEngine(backend.eng), withRunner(runner))
	if !errors.Is(err, ErrReuseMismatch) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want configuration mismatch", err)
	}
}

func TestReuseIdentityFailuresAreNotConfigurationMismatches(t *testing.T) {
	backend := reuseBackendFixtures()[0]
	for name, mutate := range map[string]func(*engineInfo){
		"foreign":            func(info *engineInfo) { delete(info.labels, managedLabel) },
		"invalid generation": func(info *engineInfo) { info.labels[creationLabel] = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			info, err := backend.eng.parseInspect(backend.inspect(backend.runningSpec()), "shared")
			if err != nil {
				t.Fatal(err)
			}
			mutate(info)
			cfg := newConfig()
			cfg.eng = backend.eng
			cfg.name = "shared"
			err = checkReuseCompat(info, "redis:7-alpine", cfg)
			if err == nil || errors.Is(err, ErrReuseMismatch) {
				t.Fatalf("got %v, want ownership/identity error only", err)
			}
		})
	}
	info, err := backend.eng.parseInspect(backend.inspect(backend.runningSpec()), "shared")
	if err != nil {
		t.Fatal(err)
	}
	cfg := newConfig()
	cfg.eng = backend.eng
	cfg.memory = "512M"
	err = checkReuseCompat(info, "redis:7-alpine", cfg)
	if !errors.Is(err, ErrReuseMismatch) || strings.Count(err.Error(), ErrReuseMismatch.Error()) != 1 {
		t.Fatalf("got %v, want one mismatch prefix", err)
	}
}
