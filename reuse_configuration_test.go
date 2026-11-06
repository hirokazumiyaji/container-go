package container

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

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
