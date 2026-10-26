package container

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

func assertOptionValidationError(t *testing.T, err error, option string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted an invalid value", option)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if validationErr.Option != option {
		t.Fatalf("validation option = %q, want %q", validationErr.Option, option)
	}
}

func TestWithShmSizeValidation(t *testing.T) {
	for _, size := range []string{"64M", "1G", "512", "2T", "1P"} {
		t.Run("valid "+size, func(t *testing.T) {
			cfg := newConfig()
			if err := WithShmSize(size)(cfg); err != nil {
				t.Fatalf("WithShmSize(%q): %v", size, err)
			}
			if cfg.shmSize != size {
				t.Fatalf("shmSize = %q, want %q", cfg.shmSize, size)
			}
		})
	}
	for _, size := range []string{"", "0", "-1", "64MB", "1.5G", "--shm-size", "1G\n", "1\x00G"} {
		t.Run("invalid "+size, func(t *testing.T) {
			err := WithShmSize(size)(newConfig())
			assertOptionValidationError(t, err, "WithShmSize")
		})
	}

	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithShmSize("0"), withRunner(f), withEngine(dockerEngine{}))
	assertOptionValidationError(t, err, "WithShmSize")
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for invalid shm size: %v", f.calls)
	}
}

func TestCapabilityOptionValidationAndNormalization(t *testing.T) {
	cfg := newConfig()
	if err := WithCapAdd("NET_ADMIN", "CAP_SYS_PTRACE", "NET_ADMIN", "CAP_NET_ADMIN")(cfg); err != nil {
		t.Fatalf("WithCapAdd: %v", err)
	}
	if want := []string{"NET_ADMIN", "CAP_SYS_PTRACE"}; !slices.Equal(cfg.capAdd, want) {
		t.Fatalf("capAdd = %v, want %v", cfg.capAdd, want)
	}
	if err := WithCapAdd("ALL")(cfg); err != nil {
		t.Fatalf("WithCapAdd(ALL): %v", err)
	}
	if got := cfg.capAdd[len(cfg.capAdd)-1]; got != "ALL" {
		t.Fatalf("ALL capability = %q", got)
	}

	if err := WithCapDrop("MKNOD", "CAP_MKNOD")(cfg); err != nil {
		t.Fatalf("WithCapDrop: %v", err)
	}
	if want := []string{"MKNOD"}; !slices.Equal(cfg.capDrop, want) {
		t.Fatalf("capDrop = %v, want %v", cfg.capDrop, want)
	}

	for _, capability := range []string{"", "CAP_ALL", "net_admin", "NET-ADMIN", "CAP_", "1ABC", "NET ADMIN", "NET_ADMIN\n"} {
		t.Run("invalid cap-add "+capability, func(t *testing.T) {
			err := WithCapAdd(capability)(newConfig())
			assertOptionValidationError(t, err, "WithCapAdd")
		})
		t.Run("invalid cap-drop "+capability, func(t *testing.T) {
			err := WithCapDrop(capability)(newConfig())
			assertOptionValidationError(t, err, "WithCapDrop")
		})
	}
}

func TestWithUlimitValidation(t *testing.T) {
	cfg := newConfig()
	if err := WithUlimit("nofile", 1024, 2048)(cfg); err != nil {
		t.Fatalf("WithUlimit: %v", err)
	}
	if err := WithUlimit("memlock", 0, 0)(cfg); err != nil {
		t.Fatalf("WithUlimit memlock: %v", err)
	}
	if want := "nofile=1024:2048"; cfg.ulimits[0].arg() != want {
		t.Fatalf("ulimit arg = %q, want %q", cfg.ulimits[0].arg(), want)
	}

	for _, tc := range []struct {
		name string
		soft int64
		hard int64
	}{
		{name: "", soft: 1, hard: 1},
		{name: "as", soft: 1, hard: 1},
		{name: "NOFILE", soft: 1, hard: 1},
		{name: "RLIMIT_NOFILE", soft: 1, hard: 1},
		{name: "nofile", soft: -1, hard: 1},
		{name: "nofile", soft: -2, hard: -1},
		{name: "nofile", soft: 1, hard: -2},
		{name: "nofile", soft: 2, hard: 1},
	} {
		t.Run("invalid "+tc.name, func(t *testing.T) {
			err := WithUlimit(tc.name, tc.soft, tc.hard)(newConfig())
			assertOptionValidationError(t, err, "WithUlimit")
		})
	}

	duplicate := newConfig()
	if err := WithUlimit("nofile", 1024, 2048)(duplicate); err != nil {
		t.Fatal(err)
	}
	assertOptionValidationError(t, WithUlimit("nofile", 1, 1)(duplicate), "WithUlimit")
}

func TestWithExtraHostValidation(t *testing.T) {
	cfg := newConfig()
	if err := WithExtraHost("api.example.com", "host-gateway")(cfg); err != nil {
		t.Fatalf("WithExtraHost host-gateway: %v", err)
	}
	if err := WithExtraHost("db", "127.0.0.1")(cfg); err != nil {
		t.Fatalf("WithExtraHost IPv4: %v", err)
	}
	if err := WithExtraHost("v6", "0:0:0:0:0:0:0:1")(cfg); err != nil {
		t.Fatalf("WithExtraHost IPv6: %v", err)
	}
	if got, want := cfg.extraHosts[2].arg(), "v6:::1"; got != want {
		t.Fatalf("IPv6 extra host arg = %q, want %q", got, want)
	}

	for _, tc := range []struct{ host, ip string }{
		{host: "", ip: "127.0.0.1"},
		{host: "-db", ip: "127.0.0.1"},
		{host: "db-", ip: "127.0.0.1"},
		{host: "bad host", ip: "127.0.0.1"},
		{host: "bad_host", ip: "127.0.0.1"},
		{host: "db", ip: ""},
		{host: "db", ip: "not-an-ip"},
		{host: "db", ip: "--privileged"},
	} {
		t.Run("invalid "+tc.host+"/"+tc.ip, func(t *testing.T) {
			err := WithExtraHost(tc.host, tc.ip)(newConfig())
			assertOptionValidationError(t, err, "WithExtraHost")
		})
	}

	duplicate := newConfig()
	if err := WithExtraHost("db", "127.0.0.1")(duplicate); err != nil {
		t.Fatal(err)
	}
	assertOptionValidationError(t, WithExtraHost("db", "127.0.0.1")(duplicate), "WithExtraHost")
	assertOptionValidationError(t, WithExtraHost("db", "127.0.0.2")(duplicate), "WithExtraHost")
}

func TestWithHostnameValidation(t *testing.T) {
	for _, name := range []string{"db", "api.example.com", "UPPER-1", "a"} {
		t.Run("valid "+name, func(t *testing.T) {
			cfg := newConfig()
			if err := WithHostname(name)(cfg); err != nil {
				t.Fatalf("WithHostname(%q): %v", name, err)
			}
			if cfg.hostname != name {
				t.Fatalf("hostname = %q, want %q", cfg.hostname, name)
			}
		})
	}
	tooLong := strings.Repeat("a", maxContainerHostnameBytes+1)
	for _, name := range []string{"", "-db", "db-", "db..example", "bad host", "bad_host", "db\n", tooLong} {
		t.Run("invalid "+name, func(t *testing.T) {
			err := WithHostname(name)(newConfig())
			assertOptionValidationError(t, err, "WithHostname")
		})
	}
}

func TestWithCPUsValidationAndFormatting(t *testing.T) {
	for _, cpus := range []float64{0.5, 1.25, 2, 0.01} {
		t.Run("valid "+formatCPUs(cpus), func(t *testing.T) {
			cfg := newConfig()
			if err := WithCPUs(cpus)(cfg); err != nil {
				t.Fatalf("WithCPUs(%v): %v", cpus, err)
			}
			if cfg.cpus != cpus {
				t.Fatalf("cpus = %v, want %v", cfg.cpus, cpus)
			}
		})
	}
	for _, cpus := range []float64{0, -0.5, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run("invalid "+formatCPUs(cpus), func(t *testing.T) {
			err := WithCPUs(cpus)(newConfig())
			assertOptionValidationError(t, err, "WithCPUs")
		})
	}

	cfg := dockerTestConfig(t, WithCPUs(0.5))
	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--cpus 0.5") {
		t.Fatalf("fractional cpus missing from Docker args: %s", joined)
	}

	cfg = dockerTestConfig(t, WithCPUs(1.25))
	joined = strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--cpus 1.25") {
		t.Fatalf("fractional cpus missing from Docker args: %s", joined)
	}

	cfg = dockerTestConfig(t, WithCPUs(2))
	joined = strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--cpus 2") || strings.Contains(joined, "2.0") {
		t.Fatalf("integer cpus formatting = %s", joined)
	}
}

func TestDockerRunArgsCarryNewRunOptions(t *testing.T) {
	cfg := dockerTestConfig(t,
		WithCPUs(0.5), WithShmSize("256M"), WithPrivileged(),
		WithCapAdd("NET_ADMIN"), WithCapDrop("MKNOD"),
		WithUlimit("nofile", 1024, 2048),
		WithExtraHost("api.example.com", "host-gateway"),
		WithHostname("db"),
	)

	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	for _, want := range []string{
		"--cpus 0.5",
		"--shm-size 256M",
		"--privileged",
		"--cap-add NET_ADMIN",
		"--cap-drop MKNOD",
		"--ulimit nofile=1024:2048",
		"--add-host api.example.com:host-gateway",
		"--hostname db",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
}

func TestAppleRejectsUnsupportedRunOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{name: "privileged", opt: WithPrivileged()},
		{name: "extra host", opt: WithExtraHost("api.example.com", "host-gateway")},
		{name: "hostname", opt: WithHostname("db")},
		{name: "fractional cpus 0.5", opt: WithCPUs(0.5)},
		{name: "fractional cpus 1.25", opt: WithCPUs(1.25)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertAppleRejectsBeforeCLI(t, tc.opt)
			if !errors.Is(err, ErrUnsupportedCapability) {
				t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
			}
		})
	}
}

func TestAppleAcceptsSupportedRunOptions(t *testing.T) {
	cfg := appleCapabilityConfig(t,
		WithCPUs(2), WithShmSize("64M"),
		WithCapAdd("NET_ADMIN"), WithCapDrop("MKNOD"),
		WithUlimit("nofile", 1024, 2048),
	)
	if err := (appleEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("Apple rejected supported run options: %v", err)
	}

	joined := strings.Join((appleEngine{}).runArgs(cfg, "redis:7-alpine", ""), " ")
	for _, want := range []string{
		"--cpus 2",
		"--shm-size 64M",
		"--cap-add NET_ADMIN",
		"--cap-drop MKNOD",
		"--ulimit nofile=1024:2048",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Apple args missing %q: %s", want, joined)
		}
	}
}

func TestReuseFlightKeyIncludesRunOptions(t *testing.T) {
	base := func() *config {
		cfg := newConfig()
		cfg.eng = dockerEngine{}
		cfg.name = "myctr"
		return cfg
	}

	plain := base()
	baseKey := reuseFlightKey(plain)

	withCPUs := base()
	if err := WithCPUs(0.5)(withCPUs); err != nil {
		t.Fatal(err)
	}
	withShm := base()
	if err := WithShmSize("128M")(withShm); err != nil {
		t.Fatal(err)
	}
	withPrivileged := base()
	if err := WithPrivileged()(withPrivileged); err != nil {
		t.Fatal(err)
	}
	withCap := base()
	if err := WithCapAdd("NET_ADMIN")(withCap); err != nil {
		t.Fatal(err)
	}
	withUlimit := base()
	if err := WithUlimit("nofile", 1, 1)(withUlimit); err != nil {
		t.Fatal(err)
	}
	withExtraHost := base()
	if err := WithExtraHost("db", "127.0.0.1")(withExtraHost); err != nil {
		t.Fatal(err)
	}
	withHostname := base()
	if err := WithHostname("db")(withHostname); err != nil {
		t.Fatal(err)
	}

	keys := map[string]string{}
	for name, cfg := range map[string]*config{
		"cpus":       withCPUs,
		"shm":        withShm,
		"privileged": withPrivileged,
		"cap-add":    withCap,
		"ulimit":     withUlimit,
		"extra-host": withExtraHost,
		"hostname":   withHostname,
	} {
		key := reuseFlightKey(cfg)
		if key == baseKey {
			t.Fatalf("%s did not change the reuse flight key", name)
		}
		for otherName, otherKey := range keys {
			if otherKey == key {
				t.Fatalf("%s and %s shared one reuse flight key", name, otherName)
			}
		}
		keys[name] = key
	}

	first := base()
	if err := WithCapAdd("NET_ADMIN", "SYS_PTRACE")(first); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("nofile", 1, 1)(first); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("memlock", 2, 2)(first); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("db", "127.0.0.1")(first); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("cache", "127.0.0.2")(first); err != nil {
		t.Fatal(err)
	}

	second := base()
	if err := WithCapAdd("SYS_PTRACE", "CAP_NET_ADMIN")(second); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("memlock", 2, 2)(second); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("nofile", 1, 1)(second); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("cache", "127.0.0.2")(second); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("db", "127.0.0.1")(second); err != nil {
		t.Fatal(err)
	}
	if firstKey, secondKey := reuseFlightKey(first), reuseFlightKey(second); firstKey != secondKey {
		t.Fatalf("option order changed the reuse flight key: %q != %q", firstKey, secondKey)
	}
}

func TestReuseRunOptionCompatibility(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "myctr"

	matching := &engineInfo{runOptions: engineRunOptions{
		cpus:            0.5,
		cpusReported:    true,
		shmSizeBytes:    256 << 20,
		shmSizeReported: true,
		privileged:      true,
		capAdd:          []string{"CAP_NET_ADMIN"},
		capDrop:         []string{"MKNOD"},
		ulimits:         []ulimitSpec{{name: "nofile", soft: 1024, hard: 2048}},
		extraHosts:      []extraHostSpec{{host: "api.example.com", ip: "host-gateway"}},
		hostname:        "db",
	}}

	if err := WithCPUs(0.5)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithShmSize("256M")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithPrivileged()(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithCapAdd("NET_ADMIN")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithCapDrop("CAP_MKNOD")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("nofile", 1024, 2048)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("api.example.com", "host-gateway")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithHostname("db")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := checkReuseRunOptions(matching, cfg); err != nil {
		t.Fatalf("matching run options rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*engineRunOptions)
		option string
	}{
		{name: "different cpus", mutate: func(o *engineRunOptions) { o.cpus = 1 }, option: "cpus"},
		{name: "cpus unreported", mutate: func(o *engineRunOptions) { o.cpusReported = false }, option: "cpus"},
		{name: "different shm", mutate: func(o *engineRunOptions) { o.shmSizeBytes = 64 << 20 }, option: "shm"},
		{name: "shm unreported", mutate: func(o *engineRunOptions) { o.shmSizeReported = false }, option: "shm"},
		{name: "not privileged", mutate: func(o *engineRunOptions) { o.privileged = false }, option: "privileged"},
		{name: "missing requested capability", mutate: func(o *engineRunOptions) { o.capAdd = nil }, option: "capabilities"},
		{name: "missing dropped capability", mutate: func(o *engineRunOptions) { o.capDrop = nil }, option: "capabilities"},
		{name: "different ulimit", mutate: func(o *engineRunOptions) { o.ulimits[0].hard = 4096 }, option: "ulimits"},
		{name: "different extra host", mutate: func(o *engineRunOptions) {
			o.extraHosts[0].ip = "127.0.0.1"
		}, option: "extra hosts"},
		{name: "different hostname", mutate: func(o *engineRunOptions) { o.hostname = "other" }, option: "hostname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mismatched := cloneEngineInfo(matching)
			tc.mutate(&mismatched.runOptions)
			err := checkReuseRunOptions(mismatched, cfg)
			if err == nil {
				t.Fatalf("%s accepted a mismatched container", tc.name)
			}
			if !strings.Contains(err.Error(), tc.option) {
				t.Fatalf("error = %v, want %q in message", err, tc.option)
			}
		})
	}
}

func TestReuseRunOptionCompatibilityOrderInsensitive(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "myctr"
	if err := WithCapAdd("NET_ADMIN", "SYS_PTRACE")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("nofile", 1, 1)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithUlimit("memlock", 2, 2)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("db", "127.0.0.1")(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithExtraHost("cache", "::1")(cfg); err != nil {
		t.Fatal(err)
	}

	info := &engineInfo{runOptions: engineRunOptions{
		capAdd:     []string{"SYS_PTRACE", "CAP_NET_ADMIN"},
		ulimits:    []ulimitSpec{{name: "memlock", soft: 2, hard: 2}, {name: "nofile", soft: 1, hard: 1}},
		extraHosts: []extraHostSpec{{host: "cache", ip: "::1"}, {host: "db", ip: "127.0.0.1"}},
	}}
	if err := checkReuseRunOptions(info, cfg); err != nil {
		t.Fatalf("order-insensitive options rejected: %v", err)
	}
}

func TestReuseCapabilityCompatibilityFollowsRequestedSubset(t *testing.T) {
	dockerCfg := func(t *testing.T, opts ...Option) *config {
		t.Helper()
		cfg := newConfig()
		cfg.eng = dockerEngine{}
		cfg.name = "myctr"
		for _, opt := range opts {
			if err := opt(cfg); err != nil {
				t.Fatal(err)
			}
		}
		return cfg
	}

	for _, tc := range []struct {
		name     string
		opts     []Option
		reported engineRunOptions
		wantErr  bool
	}{
		{
			name:     "empty request accepts non-empty inspected defaults",
			reported: engineRunOptions{capAdd: []string{"CAP_CHOWN"}, capDrop: []string{"CAP_MKNOD"}},
		},
		{
			name:     "requested add present with prefix difference",
			opts:     []Option{WithCapAdd("NET_ADMIN")},
			reported: engineRunOptions{capAdd: []string{"CAP_NET_ADMIN", "CAP_CHOWN"}},
		},
		{
			name:     "requested add missing",
			opts:     []Option{WithCapAdd("NET_ADMIN")},
			reported: engineRunOptions{capAdd: []string{"CAP_CHOWN"}},
			wantErr:  true,
		},
		{
			name:     "requested ALL present",
			opts:     []Option{WithCapAdd("ALL")},
			reported: engineRunOptions{capAdd: []string{"ALL"}},
		},
		{
			name:     "requested add satisfied by inspected ALL",
			opts:     []Option{WithCapAdd("NET_ADMIN")},
			reported: engineRunOptions{capAdd: []string{"ALL"}},
		},
		{
			name:     "requested ALL missing from explicit inspected set",
			opts:     []Option{WithCapAdd("ALL")},
			reported: engineRunOptions{capAdd: []string{"CAP_NET_ADMIN"}},
			wantErr:  true,
		},
		{
			name:     "requested drop present",
			opts:     []Option{WithCapDrop("MKNOD")},
			reported: engineRunOptions{capDrop: []string{"CAP_MKNOD"}},
		},
		{
			name:     "requested drop missing",
			opts:     []Option{WithCapDrop("MKNOD")},
			reported: engineRunOptions{capDrop: []string{"CAP_CHOWN"}},
			wantErr:  true,
		},
		{
			name:     "empty drop request accepts non-empty inspected drop",
			reported: engineRunOptions{capDrop: []string{"CAP_MKNOD"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dockerCfg(t, tc.opts...)
			err := checkReuseRunOptions(&engineInfo{runOptions: tc.reported}, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("mismatched capabilities were accepted")
				}
				if !strings.Contains(err.Error(), "capabilities") {
					t.Fatalf("error = %v, want capabilities in message", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("compatible capabilities rejected: %v", err)
			}
		})
	}
}

func TestDockerExtraHostIPv6Forms(t *testing.T) {
	parsed := parseDockerExtraHosts([]string{
		"host:2001:db8::1",
		"bracketed:[2001:db8::1]",
		"equals=2001:db8::1",
	})
	want := []extraHostSpec{
		{host: "host", ip: "2001:db8::1"},
		{host: "bracketed", ip: "2001:db8::1"},
		{host: "equals", ip: "2001:db8::1"},
	}
	if !slices.Equal(parsed, want) {
		t.Fatalf("parsed extra hosts = %v, want %v", parsed, want)
	}

	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "myctr"
	if err := WithExtraHost("host", "2001:0db8:0:0:0:0:0:1")(cfg); err != nil {
		t.Fatal(err)
	}
	for _, inspect := range [][]string{
		{"host:2001:db8::1"},
		{"host:[2001:db8::1]"},
	} {
		info := &engineInfo{runOptions: engineRunOptions{extraHosts: parseDockerExtraHosts(inspect)}}
		if err := checkReuseRunOptions(info, cfg); err != nil {
			t.Fatalf("canonical IPv6 extra host rejected against inspect %v: %v", inspect, err)
		}
	}
}

func TestDockerParseInspectRunOptions(t *testing.T) {
	data := []byte(`[{
		"Id": "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Hostname": "db", "Labels": {}},
		"HostConfig": {
			"NetworkMode": "bridge",
			"NanoCpus": 500000000,
			"ShmSize": 268435456,
			"Privileged": true,
			"CapAdd": ["CAP_NET_ADMIN"],
			"CapDrop": ["CAP_MKNOD"],
			"Ulimits": [{"Name": "nofile", "Soft": 1024, "Hard": 2048}],
			"ExtraHosts": ["api.example.com:host-gateway", "db:::1"]
		}
	}]`)

	info, err := dockerEngine{}.parseInspect(data, "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	opts := info.runOptions
	if opts.cpus != 0.5 || !opts.cpusReported {
		t.Fatalf("cpus = %v reported=%v, want 0.5 true", opts.cpus, opts.cpusReported)
	}
	if opts.shmSizeBytes != 256<<20 || !opts.shmSizeReported {
		t.Fatalf("shm = %d reported=%v", opts.shmSizeBytes, opts.shmSizeReported)
	}
	if !opts.privileged {
		t.Fatal("privileged not reported")
	}
	if !slices.Equal(opts.capAdd, []string{"CAP_NET_ADMIN"}) || !slices.Equal(opts.capDrop, []string{"CAP_MKNOD"}) {
		t.Fatalf("capabilities = %v/%v", opts.capAdd, opts.capDrop)
	}
	if !slices.Equal(opts.ulimits, []ulimitSpec{{name: "nofile", soft: 1024, hard: 2048}}) {
		t.Fatalf("ulimits = %v", opts.ulimits)
	}
	if !slices.Equal(opts.extraHosts, []extraHostSpec{
		{host: "api.example.com", ip: "host-gateway"},
		{host: "db", ip: "::1"},
	}) {
		t.Fatalf("extra hosts = %v", opts.extraHosts)
	}
	if opts.hostname != "db" {
		t.Fatalf("hostname = %q", opts.hostname)
	}
}

func TestAppleParseInspectRunOptions(t *testing.T) {
	data := []byte(`[{
		"id": "myctr",
		"configuration": {
			"image": {"reference": "redis:7-alpine"},
			"labels": {},
			"resources": {"cpus": 2},
			"capAdd": ["CAP_NET_ADMIN"],
			"capDrop": ["CAP_MKNOD"],
			"shmSize": 268435456,
			"initProcess": {"rlimits": [{"limit": "RLIMIT_NOFILE", "soft": 1024, "hard": 2048}]}
		},
		"status": {"state": "running", "networks": []}
	}]`)

	info, err := (appleEngine{}).parseInspect(data, "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	opts := info.runOptions
	if opts.cpus != 2 || !opts.cpusReported {
		t.Fatalf("cpus = %v reported=%v, want 2 true", opts.cpus, opts.cpusReported)
	}
	if opts.shmSizeBytes != 256<<20 || !opts.shmSizeReported {
		t.Fatalf("shm = %d reported=%v", opts.shmSizeBytes, opts.shmSizeReported)
	}
	if !slices.Equal(opts.capAdd, []string{"CAP_NET_ADMIN"}) || !slices.Equal(opts.capDrop, []string{"CAP_MKNOD"}) {
		t.Fatalf("capabilities = %v/%v", opts.capAdd, opts.capDrop)
	}
	if !slices.Equal(opts.ulimits, []ulimitSpec{{name: "nofile", soft: 1024, hard: 2048}}) {
		t.Fatalf("ulimits = %v", opts.ulimits)
	}
}

func TestWithUlimitUnlimited(t *testing.T) {
	for _, tc := range []struct {
		name string
		soft int64
		hard int64
		want string
	}{
		{name: "both unlimited", soft: -1, hard: -1, want: "memlock=-1:-1"},
		{name: "hard unlimited", soft: 1024, hard: -1, want: "memlock=1024:-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			if err := WithUlimit("memlock", tc.soft, tc.hard)(cfg); err != nil {
				t.Fatalf("WithUlimit: %v", err)
			}
			for _, backend := range []engine{dockerEngine{}, appleEngine{}} {
				if args := backend.runArgs(cfg, "redis:7-alpine", ""); !slices.Contains(args, tc.want) {
					t.Errorf("%s args = %v, want %q", backend.name(), args, tc.want)
				}
			}
			info := &engineInfo{runOptions: engineRunOptions{
				ulimits: []ulimitSpec{{name: "memlock", soft: tc.soft, hard: tc.hard}},
			}}
			if err := checkReuseRunOptions(info, cfg); err != nil {
				t.Fatalf("matching unlimited reuse rejected: %v", err)
			}
		})
	}
}
