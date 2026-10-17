package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestDockerEndpointSchemeClassification(t *testing.T) {
	tests := []struct {
		raw        string
		wantRemote bool
		wantHost   string
		wantScheme string
		wantErr    bool
	}{
		{raw: "tcp://10.0.0.5:2375", wantRemote: true, wantHost: "10.0.0.5", wantScheme: "tcp"},
		{raw: "tcp://remote-host:2375", wantRemote: true, wantHost: "remote-host", wantScheme: "tcp"},
		{raw: "tcp://127.0.0.1:2375", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "tcp"},
		{raw: "tcp://localhost:2375", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "tcp"},
		{raw: "tcp://:2375", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "tcp"},
		{raw: "ssh://user@remote-host", wantRemote: true, wantHost: "remote-host", wantScheme: "ssh"},
		{raw: "ssh://user@remote-host:22", wantRemote: true, wantHost: "remote-host", wantScheme: "ssh"},
		{raw: "ssh://user@10.0.0.5", wantRemote: true, wantHost: "10.0.0.5", wantScheme: "ssh"},
		{raw: "ssh://user@127.0.0.1", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "ssh"},
		{raw: "unix:///var/run/docker.sock", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "unix"},
		{raw: "npipe:////./pipe/docker_engine", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "npipe"},
		{raw: "fd://", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "fd"},
		{raw: "", wantRemote: false, wantHost: "127.0.0.1", wantScheme: "unix"},
		{raw: "custom://remote-daemon:2375", wantErr: true},
		{raw: "ftp://remote-daemon:2375", wantErr: true},
		{raw: "bogus://foo", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			ep, err := parseDockerEndpoint(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDockerEndpoint(%q) expected error, got nil", tt.raw)
				}
				if !errors.Is(err, ErrInvalidConfig) {
					t.Errorf("parseDockerEndpoint(%q) error = %v, want ErrInvalidConfig", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDockerEndpoint(%q) unexpected error: %v", tt.raw, err)
			}
			if ep.isRemote != tt.wantRemote {
				t.Errorf("ep.isRemote = %v, want %v", ep.isRemote, tt.wantRemote)
			}
			if ep.clientHost != tt.wantHost {
				t.Errorf("ep.clientHost = %q, want %q", ep.clientHost, tt.wantHost)
			}
			if ep.scheme != tt.wantScheme {
				t.Errorf("ep.scheme = %q, want %q", ep.scheme, tt.wantScheme)
			}
		})
	}
}

func TestDockerContextResolution(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves remote ssh context", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"ssh-ctx","Endpoints":{"docker":{"Host":"ssh://user@remote-box:22"}}}]`
		cfg := dockerTestConfig(t, WithDockerContext("ssh-ctx"))
		cfg.runner = f

		eng := dockerEngine{context: "ssh-ctx"}
		if err := eng.checkConfig(ctx, cfg); err != nil {
			t.Fatalf("checkConfig failed: %v", err)
		}
		if !cfg.eng.isRemote() {
			t.Errorf("cfg.eng.isRemote() = false, want true")
		}
		if got := cfg.eng.defaultHost(); got != "remote-box" {
			t.Errorf("cfg.eng.defaultHost() = %q, want remote-box", got)
		}

		args := cfg.eng.runArgs(cfg, "redis:7-alpine", "")
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "127.0.0.1::") {
			t.Errorf("runArgs should not bind loopback on remote context: %s", joined)
		}
	})

	t.Run("resolves local unix context", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"local-ctx","Endpoints":{"docker":{"Host":"unix:///custom/docker.sock"}}}]`
		cfg := dockerTestConfig(t, WithDockerContext("local-ctx"), WithExposedPorts("6379/tcp"))
		cfg.runner = f

		eng := dockerEngine{context: "local-ctx"}
		if err := eng.checkConfig(ctx, cfg); err != nil {
			t.Fatalf("checkConfig failed: %v", err)
		}
		if cfg.eng.isRemote() {
			t.Errorf("cfg.eng.isRemote() = true, want false")
		}
		if got := cfg.eng.defaultHost(); got != "127.0.0.1" {
			t.Errorf("cfg.eng.defaultHost() = %q, want 127.0.0.1", got)
		}

		args := cfg.eng.runArgs(cfg, "redis:7-alpine", "")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "127.0.0.1::6379/tcp") {
			t.Errorf("runArgs should bind loopback on local context: %s", joined)
		}
	})

	t.Run("unknown context fails closed with ConfigError", func(t *testing.T) {
		f := newDockerTestRunner()
		cfg := dockerTestConfig(t, WithDockerContext("nonexistent"))
		cfg.runner = f

		eng := dockerEngine{context: "nonexistent"}
		err := eng.checkConfig(ctx, cfg)
		if err == nil {
			t.Fatalf("checkConfig with unknown context expected error, got nil")
		}
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) {
			t.Errorf("expected *ConfigError, got %T (%v)", err, err)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("expected errors.Is(err, ErrInvalidConfig), got false")
		}
	})

	t.Run("unknown scheme in context fails closed with ConfigError", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"bad-ctx","Endpoints":{"docker":{"Host":"custom://remote:2375"}}}]`
		cfg := dockerTestConfig(t, WithDockerContext("bad-ctx"))
		cfg.runner = f

		eng := dockerEngine{context: "bad-ctx"}
		err := eng.checkConfig(ctx, cfg)
		if err == nil {
			t.Fatalf("checkConfig with unknown scheme context expected error, got nil")
		}
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) {
			t.Errorf("expected *ConfigError, got %T (%v)", err, err)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("expected errors.Is(err, ErrInvalidConfig), got false")
		}
	})
}

func TestDockerContextPublishAndRewriteMatrix(t *testing.T) {
	ctx := context.Background()

	matrix := []struct {
		name                 string
		contextJSON          string
		contextName          string
		dockerHost           string
		wantRemote           bool
		wantDefaultHost      string
		allowLoopbackPublish bool
		wantAutoPublishBind  string
	}{
		{
			name:                 "local unix context",
			contextJSON:          `[{"Name":"local","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`,
			contextName:          "local",
			wantRemote:           false,
			wantDefaultHost:      "127.0.0.1",
			allowLoopbackPublish: true,
			wantAutoPublishBind:  "127.0.0.1::",
		},
		{
			name:                 "local npipe context",
			contextJSON:          `[{"Name":"npipe","Endpoints":{"docker":{"Host":"npipe:////./pipe/docker_engine"}}}]`,
			contextName:          "npipe",
			wantRemote:           false,
			wantDefaultHost:      "127.0.0.1",
			allowLoopbackPublish: true,
			wantAutoPublishBind:  "127.0.0.1::",
		},
		{
			name:                 "remote tcp context",
			contextJSON:          `[{"Name":"tcp-remote","Endpoints":{"docker":{"Host":"tcp://remote-tcp:2375"}}}]`,
			contextName:          "tcp-remote",
			wantRemote:           true,
			wantDefaultHost:      "remote-tcp",
			allowLoopbackPublish: false,
			wantAutoPublishBind:  "0.0.0.0::",
		},
		{
			name:                 "remote ssh context",
			contextJSON:          `[{"Name":"ssh-remote","Endpoints":{"docker":{"Host":"ssh://user@remote-ssh"}}}]`,
			contextName:          "ssh-remote",
			wantRemote:           true,
			wantDefaultHost:      "remote-ssh",
			allowLoopbackPublish: false,
			wantAutoPublishBind:  "0.0.0.0::",
		},
		{
			name:                 "active remote context with empty contextName",
			contextJSON:          `[{"Name":"active-remote","Endpoints":{"docker":{"Host":"ssh://user@active-remote"}}}]`,
			contextName:          "",
			wantRemote:           true,
			wantDefaultHost:      "active-remote",
			allowLoopbackPublish: false,
			wantAutoPublishBind:  "0.0.0.0::",
		},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			f := newDockerTestRunner()
			f.contextJSON = tc.contextJSON

			cfgOpts := []Option{
				WithExposedPorts("8080/tcp"),
			}
			if tc.contextName != "" {
				cfgOpts = append(cfgOpts, WithDockerContext(tc.contextName))
			}
			cfg := dockerTestConfig(t, cfgOpts...)
			cfg.runner = f

			eng := dockerEngine{context: tc.contextName, host: tc.dockerHost}
			if err := eng.checkConfig(ctx, cfg); err != nil {
				t.Fatalf("checkConfig failed: %v", err)
			}
			if cfg.eng.isRemote() != tc.wantRemote {
				t.Errorf("isRemote() = %v, want %v", cfg.eng.isRemote(), tc.wantRemote)
			}
			if cfg.eng.defaultHost() != tc.wantDefaultHost {
				t.Errorf("defaultHost() = %q, want %q", cfg.eng.defaultHost(), tc.wantDefaultHost)
			}

			args := cfg.eng.runArgs(cfg, "redis:7-alpine", "")
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, tc.wantAutoPublishBind+"8080/tcp") {
				t.Errorf("runArgs = %s, want auto-publish binding containing %q", joined, tc.wantAutoPublishBind)
			}

			// Test loopback publish
			loopbackCfg := dockerTestConfig(t, WithPublishedPort("127.0.0.1:18080:8080"))
			loopbackCfg.runner = f
			if tc.contextName != "" {
				_ = WithDockerContext(tc.contextName)(loopbackCfg)
			}
			err := dockerEngine{context: tc.contextName, host: tc.dockerHost}.checkConfig(ctx, loopbackCfg)
			if tc.allowLoopbackPublish {
				if err != nil {
					t.Errorf("expected loopback publish to be allowed on local context, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected loopback publish to be rejected on remote context, got nil")
				} else if !errors.Is(err, ErrEndpointUnreachable) {
					t.Errorf("expected ErrEndpointUnreachable on remote loopback publish, got %v", err)
				}
			}

			// Test host/port rewrite in connectHost
			if tc.wantRemote {
				// unspecified 0.0.0.0 must rewrite to remote host
				if got := dockerConnectHost("0.0.0.0", cfg.eng); got != tc.wantDefaultHost {
					t.Errorf("dockerConnectHost(0.0.0.0) = %q, want %q", got, tc.wantDefaultHost)
				}
				// loopback binding must error out in dockerBindingConnectHost
				_, bErr := dockerBindingConnectHost(boundPort{hostAddr: "127.0.0.1", hostPort: 32768}, cfg.eng)
				if bErr == nil || !errors.Is(bErr, ErrEndpointUnreachable) {
					t.Errorf("dockerBindingConnectHost(127.0.0.1) expected ErrEndpointUnreachable, got %v", bErr)
				}
			} else {
				// local unspecified 0.0.0.0 rewrites to 127.0.0.1
				if got := dockerConnectHost("0.0.0.0", cfg.eng); got != "127.0.0.1" {
					t.Errorf("dockerConnectHost(0.0.0.0) locally = %q, want 127.0.0.1", got)
				}
			}
		})
	}
}

func TestRemoteContextDoesNotReturnLoopbackEndpoint(t *testing.T) {
	ctx := context.Background()
	f := newDockerTestRunner()
	f.contextJSON = `[{"Name":"remote-ssh","Endpoints":{"docker":{"Host":"ssh://user@remote-box:22"}}}]`
	f.inspectJSON = `[{
		"Id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"Name": "/myctr",
		"State": {"Status": "running"},
		"HostConfig": {"NetworkMode": "bridge"},
		"Config": {"Image": "redis:7-alpine"},
		"NetworkSettings": {
			"Networks": {"bridge": {"IPAddress": "172.17.0.2"}},
			"Ports": {
				"8080/tcp": [{"HostIp": "0.0.0.0", "HostPort": "32768"}]
			}
		}
	}]`

	cfg := dockerTestConfig(t, WithDockerContext("remote-ssh"), WithExposedPorts("8080/tcp"))
	cfg.runner = f
	eng := dockerEngine{context: "remote-ssh"}
	if err := eng.checkConfig(ctx, cfg); err != nil {
		t.Fatalf("checkConfig failed: %v", err)
	}

	ctr := &Container{
		id:         "myctr",
		runner:     f,
		eng:        cfg.eng,
		targetHost: cfg.eng.defaultHost(),
		exposed:    []portSpec{{port: 8080, proto: "tcp"}},
		sync:       &lockPack{uid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	endpoint, err := ctr.Endpoint(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if strings.Contains(endpoint, "127.0.0.1") || strings.Contains(endpoint, "localhost") {
		t.Errorf("remote context endpoint returned loopback: %q", endpoint)
	}
	if want := "remote-box:32768"; endpoint != want {
		t.Errorf("Endpoint = %q, want %q", endpoint, want)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "remote-box" {
		t.Errorf("Host = %q, want remote-box", host)
	}

	targetHost := ctr.TargetHost()
	if targetHost != "remote-box" {
		t.Errorf("TargetHost = %q, want remote-box", targetHost)
	}
}

func TestClientWithDockerContext(t *testing.T) {
	f := newDockerTestRunner()
	f.contextJSON = `[{"Name":"client-remote","Endpoints":{"docker":{"Host":"ssh://user@remote-client"}}}]`

	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerContext("client-remote"),
		WithClientRunner(f),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if !client.IsRemote() {
		t.Errorf("client.IsRemote() = false, want true")
	}
	if got := client.DefaultHost(); got != "remote-client" {
		t.Errorf("client.DefaultHost() = %q, want remote-client", got)
	}

	// Unknown context in NewClient returns error
	f2 := newDockerTestRunner()
	_, err = NewClient(
		WithClientBackend("docker"),
		WithClientDockerContext("nonexistent"),
		WithClientRunner(f2),
	)
	if err == nil {
		t.Fatalf("NewClient with nonexistent context expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestDockerContextPropagationToRunner(t *testing.T) {
	ctx := context.Background()

	t.Run("propagates explicit WithDockerContext to ExecRunner env", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"my-custom-ctx","Endpoints":{"docker":{"Host":"tcp://remote:2375"}}}]`

		eng := dockerEngine{context: "my-custom-ctx"}
		resolved, err := eng.resolveTarget(ctx, f)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got := resolved.effectiveContext(); got != "my-custom-ctx" {
			t.Errorf("resolved.effectiveContext() = %q, want my-custom-ctx", got)
		}

		env := resolved.targetEnv()
		found := false
		for _, e := range env {
			if e == "DOCKER_CONTEXT=my-custom-ctx" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("resolved.targetEnv() missing DOCKER_CONTEXT=my-custom-ctx, got: %v", env)
		}

		execRunner := &cli.ExecRunner{Binary: "docker"}
		cfg := &config{eng: resolved, runner: execRunner}
		applyEngineRunnerEnv(cfg)

		er, ok := cfg.runner.(*cli.ExecRunner)
		if !ok {
			t.Fatalf("cfg.runner is not *cli.ExecRunner: %T", cfg.runner)
		}
		foundRunner := false
		for _, e := range er.Env {
			if e == "DOCKER_CONTEXT=my-custom-ctx" {
				foundRunner = true
				break
			}
		}
		if !foundRunner {
			t.Errorf("cfg.runner.Env missing DOCKER_CONTEXT=my-custom-ctx, got: %v", er.Env)
		}
	})

	t.Run("propagates active context to ExecRunner env", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"active-remote","Endpoints":{"docker":{"Host":"tcp://remote:2375"}}}]`

		eng := dockerEngine{}
		resolved, err := eng.resolveTarget(ctx, f)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got := resolved.effectiveContext(); got != "active-remote" {
			t.Errorf("resolved.effectiveContext() = %q, want active-remote", got)
		}

		env := resolved.targetEnv()
		found := false
		for _, e := range env {
			if e == "DOCKER_CONTEXT=active-remote" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("resolved.targetEnv() missing DOCKER_CONTEXT=active-remote, got: %v", env)
		}
	})

	t.Run("context takes precedence over host when both are configured", func(t *testing.T) {
		f := newDockerTestRunner()
		// Host is set to local loopback, but context points to remote box
		f.contextJSON = `[{"Name":"remote-ctx","Endpoints":{"docker":{"Host":"tcp://remote-box:2375"}}}]`

		eng := dockerEngine{
			host:    "tcp://127.0.0.1:2375",
			context: "remote-ctx",
		}
		resolved, err := eng.resolveTarget(ctx, f)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if !resolved.isRemote() {
			t.Errorf("expected isRemote=true when context is remote even if host is loopback")
		}
		if got := resolved.defaultHost(); got != "remote-box" {
			t.Errorf("resolved.defaultHost() = %q, want remote-box", got)
		}
		// targetEnv should contain DOCKER_CONTEXT and not DOCKER_HOST
		env := resolved.targetEnv()
		hasContext := false
		hasHost := false
		for _, e := range env {
			if e == "DOCKER_CONTEXT=remote-ctx" {
				hasContext = true
			}
			if strings.HasPrefix(e, "DOCKER_HOST=") {
				hasHost = true
			}
		}
		if !hasContext {
			t.Errorf("resolved.targetEnv() missing DOCKER_CONTEXT=remote-ctx, got: %v", env)
		}
		if hasHost {
			t.Errorf("resolved.targetEnv() should not pass DOCKER_HOST when context takes precedence, got: %v", env)
		}
	})

	t.Run("explicit host option takes precedence over ambient context in env", func(t *testing.T) {
		t.Setenv("DOCKER_CONTEXT", "ambient-remote-ctx")
		t.Setenv("DOCKER_HOST", "tcp://ambient-host:2375")

		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"ambient-remote-ctx","Endpoints":{"docker":{"Host":"tcp://ambient-ctx-box:2375"}}}]`

		// Explicit host option passed by user
		eng := dockerEngine{
			host:         "tcp://10.1.2.3:2375",
			explicitHost: true,
		}
		resolved, err := eng.resolveTarget(ctx, f)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got := resolved.defaultHost(); got != "10.1.2.3" {
			t.Errorf("resolved.defaultHost() = %q, want 10.1.2.3", got)
		}
		env := resolved.targetEnv()
		hasHost := false
		hasContext := false
		for _, e := range env {
			if e == "DOCKER_HOST=tcp://10.1.2.3:2375" {
				hasHost = true
			}
			if strings.HasPrefix(e, "DOCKER_CONTEXT=") {
				hasContext = true
			}
		}
		if !hasHost {
			t.Errorf("resolved.targetEnv() missing DOCKER_HOST=tcp://10.1.2.3:2375, got: %v", env)
		}
		if hasContext {
			t.Errorf("resolved.targetEnv() should not pass DOCKER_CONTEXT when host is explicitly configured, got: %v", env)
		}
	})

	t.Run("per-run options retain inherited configDir and platform", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", "/custom/profile/config")
		t.Setenv("DOCKER_DEFAULT_PLATFORM", "linux/arm64")

		cfg := &config{}
		if err := WithDockerContext("remote-box")(cfg); err != nil {
			t.Fatalf("WithDockerContext: %v", err)
		}

		eng := dockerEngine{}
		if cfg.dockerContext != nil {
			eng.context = *cfg.dockerContext
			eng.explicitContext = true
		}

		if got := eng.effectiveConfigDir(); got != "/custom/profile/config" {
			t.Errorf("effectiveConfigDir() = %q, want /custom/profile/config", got)
		}
		if got := eng.effectiveDefaultPlatform(); got != "linux/arm64" {
			t.Errorf("effectiveDefaultPlatform() = %q, want linux/arm64", got)
		}
	})

	t.Run("active context inspection error fails closed", func(t *testing.T) {
		f := newDockerTestRunner()
		f.failPrefix = "context"
		f.failStderr = "error: active context is corrupted or unavailable"

		eng := dockerEngine{}
		_, err := eng.resolveTarget(ctx, f)
		if err == nil {
			t.Fatalf("resolveTarget with failing active context expected error, got nil")
		}
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) {
			t.Errorf("expected *ConfigError on active context inspection failure, got %T (%v)", err, err)
		}
	})

	t.Run("preserves runner environment DOCKER_HOST override", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "")
		execRunner := &cli.ExecRunner{
			Binary: "docker",
			Env:    []string{"DOCKER_HOST=tcp://runner-host:2375"},
		}

		eng := dockerEngine{}
		resolved, err := eng.resolveTarget(ctx, execRunner)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got := resolved.effectiveHost(); got != "tcp://runner-host:2375" {
			t.Errorf("resolved.effectiveHost() = %q, want tcp://runner-host:2375", got)
		}
		if !resolved.isRemote() {
			t.Errorf("resolved.isRemote() = false, want true")
		}
		if got := resolved.defaultHost(); got != "runner-host" {
			t.Errorf("resolved.defaultHost() = %q, want runner-host", got)
		}

		env := resolved.targetEnv()
		hasHost := false
		for _, e := range env {
			if e == "DOCKER_HOST=tcp://runner-host:2375" {
				hasHost = true
			}
		}
		if !hasHost {
			t.Errorf("resolved.targetEnv() missing DOCKER_HOST=tcp://runner-host:2375, got: %v", env)
		}
	})

	t.Run("preserves runner environment DOCKER_CONTEXT override", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"runner-ctx","Endpoints":{"docker":{"Host":"ssh://user@remote-box"}}}]`
		f.env = []string{"DOCKER_CONTEXT=runner-ctx"}

		eng := dockerEngine{}
		resolved, err := eng.resolveTarget(ctx, f)
		if err != nil {
			t.Fatalf("resolveTarget: %v", err)
		}
		if got := resolved.effectiveContext(); got != "runner-ctx" {
			t.Errorf("resolved.effectiveContext() = %q, want runner-ctx", got)
		}
		if !resolved.isRemote() {
			t.Errorf("resolved.isRemote() = false, want true")
		}
		if got := resolved.defaultHost(); got != "remote-box" {
			t.Errorf("resolved.defaultHost() = %q, want remote-box", got)
		}

		env := resolved.targetEnv()
		hasCtx := false
		for _, e := range env {
			if e == "DOCKER_CONTEXT=runner-ctx" {
				hasCtx = true
			}
		}
		if !hasCtx {
			t.Errorf("resolved.targetEnv() missing DOCKER_CONTEXT=runner-ctx, got: %v", env)
		}
	})
}
