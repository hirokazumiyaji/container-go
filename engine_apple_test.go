package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func appleCapabilityConfig(t *testing.T, opts ...Option) *config {
	t.Helper()
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.name = "myctr"
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	return cfg
}

func assertAppleRejectsBeforeCLI(t *testing.T, opts ...Option) error {
	t.Helper()
	f := newTestRunner()
	opts = append([]Option{WithName("myctr"), withRunner(f), withEngine(appleEngine{})}, opts...)
	_, err := Run(context.Background(), "redis:7-alpine", opts...)
	if err == nil {
		t.Fatal("want capability validation error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("CLI was called despite invalid Apple config: %v", f.calls)
	}
	return err
}

func TestAppleCapabilityValidationPrecedesCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{name: "bare platform", opt: WithPlatform("linux")},
		{name: "non-Linux platform", opt: WithPlatform("windows/amd64")},
		{name: "invalid platform variant", opt: WithPlatform("linux/arm64/v7")},
		{name: "container name", opt: WithName("a")},
		{name: "network name", opt: WithNetwork("INVALID")},
		{name: "published port", opt: WithPublishedPort("1:80")},
		{name: "memory", opt: WithMemory("1K")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = assertAppleRejectsBeforeCLI(t, tc.opt)
		})
	}
}

func TestAppleCheckConfigPlatform(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		wantErr  string
	}{
		{name: "default"},
		{name: "linux amd64", platform: "linux/amd64"},
		{name: "linux arm64", platform: "linux/arm64"},
		{name: "linux arm v7", platform: "linux/arm/v7"},
		{name: "linux arm v8", platform: "linux/arm/v8"},
		{name: "armhf v7", platform: "linux/armhf/v7"},
		{name: "armel v6", platform: "linux/armel/v6"},
		{name: "aarch64 8", platform: "linux/aarch64/8"},
		{name: "amd64 v1", platform: "linux/amd64/v1"},
		{name: "bare linux", platform: "linux", wantErr: "os/arch"},
		{name: "windows", platform: "windows/amd64", wantErr: "only linux"},
		{name: "freebsd", platform: "freebsd/amd64", wantErr: "only linux"},
		{name: "arm64 v7", platform: "linux/arm64/v7", wantErr: "not valid"},
		{name: "arm v9", platform: "linux/arm/v9", wantErr: "not valid"},
		{name: "amd64 v2", platform: "linux/amd64/v2", wantErr: "not valid"},
		{name: "x86 v3", platform: "linux/x86_64/v3", wantErr: "not valid"},
		{name: "ppc64le variant", platform: "linux/ppc64le/v1", wantErr: "not valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := appleCapabilityConfig(t)
			if tc.platform != "" {
				if err := WithPlatform(tc.platform)(cfg); err != nil {
					t.Fatalf("WithPlatform(%q): %v", tc.platform, err)
				}
			}
			err := (appleEngine{}).checkConfig(cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("checkConfig(%q) = %v, want error containing %q", tc.platform, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkConfig(%q) = %v", tc.platform, err)
			}
		})
	}
}

func TestAppleCheckConfigUsesDefaultPlatformEnvironment(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "windows/amd64")
	cfg := appleCapabilityConfig(t)
	if err := (appleEngine{}).checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("checkConfig with non-Linux default = %v, want Linux capability error", err)
	}

	t.Setenv(defaultPlatformEnv, "linux/arm64")
	cfg = appleCapabilityConfig(t)
	if err := (appleEngine{}).checkConfig(cfg); err != nil {
		t.Fatalf("checkConfig with Linux default = %v", err)
	}
	if cfg.platform != "linux/arm64" {
		t.Fatalf("effective platform = %q, want linux/arm64", cfg.platform)
	}

	t.Setenv(defaultPlatformEnv, "linux/arm64/v7")
	if err := (appleEngine{}).checkConfig(appleCapabilityConfig(t)); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("checkConfig with invalid variant default = %v", err)
	}
}

func TestAppleCheckConfigContainerNames(t *testing.T) {
	valid := []string{
		"ab",
		"A1",
		"my-container_1.2",
		strings.Repeat("a", 63),
	}
	for _, name := range valid {
		cfg := appleCapabilityConfig(t, WithName(name))
		if err := (appleEngine{}).checkConfig(cfg); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
	}

	for _, name := range []string{"a"} {
		cfg := appleCapabilityConfig(t, WithName(name))
		if err := (appleEngine{}).checkConfig(cfg); err == nil {
			t.Errorf("name %q: want Apple name validation error", name)
		}
	}

	// Leading separators are rejected by the shared option validator before
	// the Apple-specific check; trailing separators are valid upstream.
	if err := WithName("-a")(appleCapabilityConfig(t)); err == nil {
		t.Error("leading-separator name: want shared validation error")
	}
	for _, name := range []string{"a-", "a."} {
		cfg := appleCapabilityConfig(t, WithName(name))
		if err := (appleEngine{}).checkConfig(cfg); err != nil {
			t.Errorf("trailing-separator name %q: %v", name, err)
		}
	}

	cfg := appleCapabilityConfig(t)
	cfg.name = strings.Repeat("a", 64)
	if err := (appleEngine{}).checkConfig(cfg); err == nil {
		t.Error("64-character name: want Apple name validation error")
	}
}

func TestAppleCheckConfigNetworkNames(t *testing.T) {
	for _, name := range []string{"a", "default", "my-network_1.example"} {
		cfg := appleCapabilityConfig(t, WithNetwork(name))
		if err := (appleEngine{}).checkConfig(cfg); err != nil {
			t.Errorf("network %q: %v", name, err)
		}
	}

	for _, name := range []string{"A", "a-", "a."} {
		cfg := appleCapabilityConfig(t, WithNetwork(name))
		if err := (appleEngine{}).checkConfig(cfg); err == nil {
			t.Errorf("network %q: want Apple network validation error", name)
		}
	}
	if err := WithNetwork("-a")(appleCapabilityConfig(t)); err == nil {
		t.Error("leading-separator network: want shared validation error")
	}

	// WithNetwork intentionally accepts a name, not the CLI's
	// name[,mac=...][,mtu=...] property syntax.
	cfg := appleCapabilityConfig(t)
	cfg.network = "default,mac=02:42:ac:11:00:02"
	if err := (appleEngine{}).checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "comma") {
		t.Fatalf("network properties = %v, want separator rejection", err)
	}
	if err := WithNetwork("default,mac=02:42:ac:11:00:02")(cfg); err == nil {
		t.Fatal("WithNetwork accepted comma-separated properties")
	}
}

func TestAppleCheckConfigPorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{name: "host port one", opt: WithPublishedPort("1:80")},
		{name: "container port one", opt: WithPublishedPort("80:1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := appleCapabilityConfig(t, tc.opt)
			if err := (appleEngine{}).checkConfig(cfg); err == nil {
				t.Fatal("want Apple port validation error")
			}
		})
	}

	cfg := appleCapabilityConfig(t,
		// Apple uses direct container IPs, so an exposed declaration is
		// metadata and does not become a --publish flag; port 1 remains
		// meaningful for the library's endpoint/wait helpers.
		WithExposedPorts("1/tcp", "2/tcp", "65535/udp"),
		WithPublishedPort("2:80/tcp"),
	)
	if err := (appleEngine{}).checkConfig(cfg); err != nil {
		t.Fatalf("valid ports: %v", err)
	}

	cfg = appleCapabilityConfig(t,
		WithPublishedPort("2:80/tcp"),
		WithPublishedPort("2:81/tcp"),
	)
	if err := (appleEngine{}).checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping publishes = %v, want overlap error", err)
	}
}

func TestAppleCheckConfigPublishedPortCount(t *testing.T) {
	publish := func(n int) []Option {
		opts := make([]Option, n)
		for i := range opts {
			opts[i] = WithPublishedPort(fmt.Sprintf("%d:80/tcp", 10000+i))
		}
		return opts
	}

	if err := (appleEngine{}).checkConfig(appleCapabilityConfig(t, publish(applePublishedPortLimit)...)); err != nil {
		t.Fatalf("%d descriptors: %v", applePublishedPortLimit, err)
	}
	if err := (appleEngine{}).checkConfig(appleCapabilityConfig(t, publish(applePublishedPortLimit+1)...)); err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("%d descriptors: want publish-count error", applePublishedPortLimit+1)
	}
	_ = assertAppleRejectsBeforeCLI(t, publish(applePublishedPortLimit+1)...)
}

func TestAppleCheckConfigMemory(t *testing.T) {
	for _, size := range []string{"200M", "1G", "1T", "1P", "209715200"} {
		cfg := appleCapabilityConfig(t, WithMemory(size))
		if err := (appleEngine{}).checkConfig(cfg); err != nil {
			t.Errorf("memory %q: %v", size, err)
		}
	}

	for _, size := range []string{"1K", "199M", "0M", "0", "9223372036854775807P", "999999999999999999999P"} {
		cfg := appleCapabilityConfig(t, WithMemory(size))
		if err := (appleEngine{}).checkConfig(cfg); err == nil {
			t.Errorf("memory %q: want Apple memory validation error", size)
		}
	}
	if _, err := appleMemoryBytes("18446744073709551616"); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("byte overflow = %v, want overflow error", err)
	}
}

func TestAppleDefaultPlatformValidationPrecedesImageWork(t *testing.T) {
	for _, platform := range []string{"linux", "linux/arm64/v7", "windows/amd64"} {
		t.Run(platform, func(t *testing.T) {
			t.Setenv(defaultPlatformEnv, platform)
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
			if err == nil {
				t.Fatalf("Run accepted invalid Apple default platform %q", platform)
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid Apple default reached image work: %v", f.calls)
			}
		})
	}
}

func TestApplePullNeverIsUnsupported(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrPullNeverUnsupported) {
		t.Fatalf("error = %v, want ErrPullNeverUnsupported", err)
	}
	if !strings.Contains(err.Error(), "PullMissing") || !strings.Contains(err.Error(), "PullAlways") {
		t.Errorf("error does not document fallback: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("PullNever reached CLI: %v", f.calls)
	}
}

func TestApplePullMissingIsDocumentedFallback(t *testing.T) {
	f := newTestRunner()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing), withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("PullMissing fallback: %v", err)
	}
	if ctr == nil || f.pullCalls != 1 {
		t.Fatalf("PullMissing fallback did not pull once: ctr=%v pulls=%d", ctr, f.pullCalls)
	}
}
