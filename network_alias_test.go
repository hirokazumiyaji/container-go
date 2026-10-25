package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestWithNetworkAliasesValidation(t *testing.T) {
	valid := []string{"db", "postgres", "a", "A1", "db-1", strings.Repeat("a", 63)}
	for i, alias := range valid {
		t.Run(fmt.Sprintf("valid-%d", i), func(t *testing.T) {
			cfg := newConfig()
			if err := WithNetworkAliases(alias)(cfg); err != nil {
				t.Fatalf("WithNetworkAliases(%q): %v", alias, err)
			}
			if got := cfg.networkAliases; !slices.Equal(got, []string{alias}) {
				t.Fatalf("stored aliases = %v, want [%q]", got, alias)
			}
		})
	}

	invalid := []string{
		"",
		"-db",
		"db-",
		"db_1",
		"db.local",
		"db x",
		"db\tx",
		"db\rx",
		"db\nx",
		"db\x00x",
		"db:1",
		"db/1",
		"db.",
		".db",
		strings.Repeat("a", 64),
	}
	for i, alias := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			cfg := newConfig()
			err := WithNetworkAliases(alias)(cfg)
			if err == nil {
				t.Fatalf("WithNetworkAliases(%q) succeeded, want validation error", alias)
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if validationErr.Option != "WithNetworkAliases" || validationErr.Field != "alias" {
				t.Fatalf("validation option/field = %q/%q, want WithNetworkAliases/alias", validationErr.Option, validationErr.Field)
			}
			if validationErr.Value != alias {
				t.Fatalf("validation value = %#v, want %q", validationErr.Value, alias)
			}
			if len(cfg.networkAliases) != 0 {
				t.Fatalf("rejected alias was stored: %v", cfg.networkAliases)
			}
		})
	}
}

func TestRunRejectsInvalidNetworkAliasesBeforeBackend(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithNetwork("test-net"), WithNetworkAliases("-db"),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("Run accepted an invalid network alias")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %T %v, want *ValidationError matching ErrInvalidOption", err, err)
	}
	if validationErr.Option != "WithNetworkAliases" || validationErr.Field != "alias" {
		t.Fatalf("validation option/field = %q/%q, want WithNetworkAliases/alias", validationErr.Option, validationErr.Field)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called before alias validation: %v", f.calls)
	}
}

func TestWithNetworkAliasesRejectsDuplicates(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{name: "same call", opts: []Option{WithNetworkAliases("db", "db")}},
		{name: "separate calls", opts: []Option{WithNetworkAliases("db"), WithNetworkAliases("db")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			var err error
			for _, opt := range tc.opts {
				if err = opt(cfg); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("duplicate alias was accepted")
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if validationErr.Option != "WithNetworkAliases" || validationErr.Field != "alias" || validationErr.Value != "db" {
				t.Fatalf("validation metadata = %#v, want WithNetworkAliases/alias/db", validationErr)
			}
			if !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("error = %v, want duplicate alias detail", err)
			}
		})
	}
}

func TestDockerRunArgsCarryNetworkAliases(t *testing.T) {
	cfg := dockerTestConfig(t, WithNetwork("test-net"), WithNetworkAliases("db", "postgres"))

	args := dockerEngine{}.runArgs(cfg, "redis:7-alpine", "")
	if got := argvValue(args, "--network"); got != "test-net" {
		t.Errorf("--network = %q, want test-net (args=%v)", got, args)
	}
	if got := argvValues(args, "--network-alias"); !slices.Equal(got, []string{"db", "postgres"}) {
		t.Errorf("--network-alias = %v, want [db postgres] (args=%v)", got, args)
	}
}

func TestDockerRunPassesNetworkAliasesToCLI(t *testing.T) {
	f := &dockerRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithNetwork("test-net"), WithNetworkAliases("db", "postgres"),
		withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := f.callWith("run")
	if run == nil {
		t.Fatalf("run was not called: %v", f.calls)
	}
	if got := argvValue(run, "--network"); got != "test-net" {
		t.Errorf("--network = %q, want test-net (run=%v)", got, run)
	}
	if got := argvValues(run, "--network-alias"); !slices.Equal(got, []string{"db", "postgres"}) {
		t.Errorf("--network-alias = %v, want [db postgres] (run=%v)", got, run)
	}
}

func TestDockerRunArgsSkipAliasesWithoutExplicitNetwork(t *testing.T) {
	cfg := dockerTestConfig(t, WithNetworkAliases("db"))
	if got := argvValues(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), "--network-alias"); len(got) != 0 {
		t.Fatalf("--network-alias = %v, want none without --network", got)
	}
}

func TestDockerRejectsNetworkAliasesOnNonUserDefinedNetworks(t *testing.T) {
	cases := []struct {
		name     string
		network  string
		explicit bool
	}{
		{name: "omitted"},
		{name: "host", network: dockerNetworkHost, explicit: true},
		{name: "none", network: dockerNetworkNone, explicit: true},
		{name: "default bridge", network: dockerNetworkBridge, explicit: true},
		{name: "default mode", network: dockerNetworkDefault, explicit: true},
		{name: "windows default", network: dockerNetworkNAT, explicit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			opts := []Option{WithName("myctr"), WithNetworkAliases("db")}
			if tc.explicit {
				opts = append(opts, WithNetwork(tc.network))
			}
			opts = append(opts, withRunner(f), withEngine(dockerEngine{}))

			_, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err == nil {
				t.Fatal("network aliases were accepted on a network that does not resolve them")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error = %T %v, want *ConfigError", err, err)
			}
			if configErr.Backend != "docker" || configErr.Option != "WithNetworkAliases" {
				t.Fatalf("ConfigError = %#v, want docker/WithNetworkAliases", configErr)
			}
			if !strings.Contains(err.Error(), "user-defined network") {
				t.Fatalf("error = %v, want user-defined network detail", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called before alias validation: %v", f.calls)
			}
		})
	}
}

func TestDockerAcceptsNetworkAliasesOnUserDefinedNetwork(t *testing.T) {
	cfg := dockerTestConfig(t,
		WithName("myctr"), WithNetwork("test-net"), WithNetworkAliases("db"))
	if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("checkConfig: %v", err)
	}
}

func TestAppleRejectsNetworkAliasesBeforeBackend(t *testing.T) {
	for _, network := range []string{"", "mynet"} {
		t.Run("network="+network, func(t *testing.T) {
			f := newTestRunner()
			opts := []Option{WithName("myctr"), WithNetworkAliases("db")}
			if network != "" {
				opts = append(opts, WithNetwork(network))
			}
			opts = append(opts, withRunner(f), withEngine(appleEngine{}))

			_, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err == nil {
				t.Fatal("Apple accepted network aliases")
			}
			if !errors.Is(err, ErrUnsupportedCapability) {
				t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("Apple backend was called before alias rejection: %v", f.calls)
			}
		})
	}
}

func TestDockerInspectReportsNetworkAliases(t *testing.T) {
	data := fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"HostConfig": {"NetworkMode": "test-net"},
		"NetworkSettings": {"Networks": {"test-net": {
			"IPAddress": "172.20.0.2",
			"Aliases": ["myctr", "db"],
			"DNSNames": ["myctr", "db", "abc123"]
		}}}
	}]`, dockerFixtureID)

	info, err := (dockerEngine{}).parseInspect([]byte(data), "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	for _, alias := range []string{"myctr", "db", "abc123"} {
		if !slices.Contains(info.networkAliases, alias) {
			t.Errorf("networkAliases = %v, want %q", info.networkAliases, alias)
		}
	}
}

func TestReuseFlightKeyIncludesNetworkAliases(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "myctr"
	cfg.network = "test-net"
	cfg.networkExplicit = true

	base := reuseFlightKey(cfg)
	cfg.networkAliases = []string{"db"}
	withDB := reuseFlightKey(cfg)
	cfg.networkAliases = []string{"cache"}
	withCache := reuseFlightKey(cfg)
	if base == withDB {
		t.Fatal("network alias did not change the reuse flight key")
	}
	if withDB == withCache {
		t.Fatal("different network aliases shared one reuse flight key")
	}

	cfg.networkAliases = []string{"db", "cache"}
	first := reuseFlightKey(cfg)
	cfg.networkAliases = []string{"cache", "db"}
	second := reuseFlightKey(cfg)
	if first != second {
		t.Fatalf("alias order changed the reuse flight key: %q != %q", first, second)
	}
}

func TestDockerReuseNetworkAliasCompatibility(t *testing.T) {
	cases := []struct {
		name     string
		reported []string
		wantErr  bool
	}{
		{name: "matching", reported: []string{"myctr", "db"}},
		{name: "extra reported aliases", reported: []string{"abc123", "cache", "db", "myctr"}},
		{name: "missing requested alias", reported: []string{"myctr", "cache"}, wantErr: true},
		{name: "no reported aliases", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dockerTestConfig(t,
				WithName("myctr"), WithReuse(), WithNetwork("test-net"), WithNetworkAliases("db"))
			info := &engineInfo{
				labels: map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: "0123456789abcdef",
				},
				uid:            dockerFixtureID,
				image:          "redis:7-alpine",
				networkMode:    "test-net",
				networkNames:   []string{"test-net"},
				networkAliases: tc.reported,
			}

			err := checkReuseCompat(info, "redis:7-alpine", cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("reuse accepted a container without the requested alias")
				}
				if !errors.Is(err, ErrNetworkMismatch) {
					t.Fatalf("error = %v, want ErrNetworkMismatch", err)
				}
				if !strings.Contains(err.Error(), `"db"`) {
					t.Fatalf("error = %v, want the missing alias named", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkReuseCompat: %v", err)
			}
		})
	}
}

// dockerReuseAliasRunner serves a Docker-shaped inspect payload after the
// first successful run. Before that, inspect reports the container missing so
// the reuse ensure path creates it. The reported creation label must match the
// generation the library put on the run, or reuse treats the container as a
// replacement.
type dockerReuseAliasRunner struct {
	*dockerRunner
	created bool
	aliases []string
}

func (r *dockerReuseAliasRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.calls = append(r.calls, args)
		if !r.created {
			id := args[len(args)-1]
			return nil, nil, &cli.CLIError{
				Binary: "docker", Args: args, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: " + id,
			}
		}
		r.mu.Lock()
		creation := r.creations["myctr"]
		r.mu.Unlock()
		if creation == "" {
			creation = "0123456789abcdef"
		}
		aliases := make([]string, 0, len(r.aliases)+1)
		aliases = append(aliases, "myctr")
		aliases = append(aliases, r.aliases...)
		aliasesJSON, err := json.Marshal(aliases)
		if err != nil {
			return nil, nil, err
		}
		return []byte(fmt.Sprintf(`[{
			"Id": %q,
			"Image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"Name": "/myctr",
			"State": {"Status": "running"},
			"Config": {"Image": "redis:7-alpine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "Labels": {
				%q: "true",
				%q: "true",
				%q: %q
			}},
			"HostConfig": {"NetworkMode": "test-net"},
			"NetworkSettings": {"Networks": {"test-net": {
				"IPAddress": "172.20.0.2",
				"Aliases": %s,
				"DNSNames": %s
			}}}
		}]`, dockerFixtureID, managedLabel, reuseLabel, creationLabel, creation, aliasesJSON, aliasesJSON)), nil, nil
	}
	if args[0] == "run" {
		r.mu.Lock()
		if r.creations == nil {
			r.creations = make(map[string]string)
		}
		name := ""
		for i, arg := range args {
			if arg == "--name" && i+1 < len(args) {
				name = args[i+1]
			}
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok && name != "" {
					r.creations[name] = value
				}
			}
		}
		r.created = true
		r.mu.Unlock()
	}
	return r.dockerRunner.Run(ctx, args...)
}

func TestDockerReuseDoesNotAttachWithDifferentNetworkAliases(t *testing.T) {
	r := &dockerReuseAliasRunner{
		dockerRunner: &dockerRunner{fakeRunner: newTestRunner()},
		aliases:      []string{"db"},
	}
	r.imagePresent = true

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithNetwork("test-net"), WithNetworkAliases("db"),
		withRunner(r), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("first reuse Run: %v", err)
	}
	if r.callWith("run") == nil {
		t.Fatal("first reuse Run did not create the container")
	}

	r.calls = nil
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithNetwork("test-net"), WithNetworkAliases("cache"),
		withRunner(r), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("reuse attached a container that does not carry the requested alias")
	}
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("error = %v, want ErrNetworkMismatch", err)
	}
	if run := r.callWith("run"); run != nil {
		t.Fatalf("second reuse Run created another container: %v", run)
	}
}
