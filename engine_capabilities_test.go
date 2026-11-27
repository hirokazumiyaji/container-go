package container

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestEngineCapabilitiesDeclareBackendContract pins each backend's declared
// capabilities. A new flag must be set deliberately for both backends, and a
// new backend must publish its own contract instead of editing shared
// branches.
func TestEngineCapabilitiesDeclareBackendContract(t *testing.T) {
	wantDocker := engineCapabilities{
		immutableID:          true,
		defaultNetwork:       true,
		copyOutVersionCheck:  true,
		healthcheck:          true,
		pruneLabelListFilter: true,
	}
	if got := (dockerEngine{}).capabilities(); got != wantDocker {
		t.Errorf("docker capabilities = %+v, want %+v", got, wantDocker)
	}

	wantApple := engineCapabilities{
		nameAddressed:          true,
		pruneCandidateMetadata: true,
		strictImageIdentity:    true,
	}
	if got := (appleEngine{}).capabilities(); got != wantApple {
		t.Errorf("apple capabilities = %+v, want %+v", got, wantApple)
	}
}

// TestOptionalEngineRolesAreDeclaredByType pins which backend supplies each
// optional role. Shared code asserts these interfaces instead of calling no-op
// methods, so dropping or adding a role must change this contract
// deliberately.
func TestOptionalEngineRolesAreDeclaredByType(t *testing.T) {
	tests := []struct {
		name      string
		eng       engine
		lifecycle bool
		copyOut   bool
		verifier  bool
		network   bool
	}{
		// Apple has no native restart, pause, copy-out, or network model.
		{name: "apple", eng: appleEngine{}, verifier: true},
		// Docker supplies every optional role; its identity verification is
		// the shared default, so it has no verifier.
		{name: "docker", eng: dockerEngine{}, lifecycle: true, copyOut: true, network: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.eng.(engineLifecycleExtensions); ok != tc.lifecycle {
				t.Errorf("engineLifecycleExtensions = %v, want %v", ok, tc.lifecycle)
			}
			if _, ok := tc.eng.(engineCopyOut); ok != tc.copyOut {
				t.Errorf("engineCopyOut = %v, want %v", ok, tc.copyOut)
			}
			if _, ok := tc.eng.(engineImageVerifier); ok != tc.verifier {
				t.Errorf("engineImageVerifier = %v, want %v", ok, tc.verifier)
			}
			if _, ok := tc.eng.(engineNetworkCompat); ok != tc.network {
				t.Errorf("engineNetworkCompat = %v, want %v", ok, tc.network)
			}
		})
	}
}

// TestTargetScopeIsExpressedByType pins that a backend without an externally
// addressable target reports none: shared code branches on a nil engineTarget
// instead of every backend supplying no-op implementations.
func TestTargetScopeIsExpressedByType(t *testing.T) {
	if target := (appleEngine{}).target(); target != nil {
		t.Fatalf("apple target() = %T, want nil", target)
	}
	if target := (dockerEngine{}).target(); target == nil {
		t.Fatal("docker target() = nil, want a target scope")
	}
}

// TestEngineCapabilityInvariants pins the combinations shared policy relies
// on. A third backend that declares both addressing modes, or a name-addressed
// prune without list metadata, would silently skip identity checks or delete
// nothing.
func TestEngineCapabilityInvariants(t *testing.T) {
	for name, caps := range map[string]engineCapabilities{
		"docker": (dockerEngine{}).capabilities(),
		"apple":  (appleEngine{}).capabilities(),
	} {
		if caps.immutableID == caps.nameAddressed {
			t.Errorf("%s: immutableID=%v nameAddressed=%v must be mutually exclusive", name, caps.immutableID, caps.nameAddressed)
		}
		if caps.pruneCandidateMetadata && !caps.nameAddressed {
			t.Errorf("%s: pruneCandidateMetadata requires nameAddressed", name)
		}
		if !caps.pruneCandidateMetadata && !caps.immutableID {
			t.Errorf("%s: an ID-addressed prune requires immutableID", name)
		}
	}
}

// TestSharedCommandBuildersBuildBackendArgv keeps both engines on the shared
// argv builders the DRY refactor introduced.
func TestSharedCommandBuildersBuildBackendArgv(t *testing.T) {
	execCfg := &execConfig{user: "1000", workdir: "/work"}
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"docker start", (dockerEngine{}).startArgs("ctr"), []string{"start", "ctr"}},
		{"apple start", (appleEngine{}).startArgs("ctr"), []string{"start", "ctr"}},
		{"docker kill", (dockerEngine{}).killArgs("ctr", "SIGTERM"), []string{"kill", "--signal", "SIGTERM", "ctr"}},
		{"apple kill", (appleEngine{}).killArgs("ctr", "SIGTERM"), []string{"kill", "--signal", "SIGTERM", "ctr"}},
		{"docker kill default", (dockerEngine{}).killArgs("ctr", ""), []string{"kill", "ctr"}},
		{"apple kill default", (appleEngine{}).killArgs("ctr", ""), []string{"kill", "ctr"}},
		{"docker copy in", (dockerEngine{}).copyToArgs("ctr", "/host", "/ctr"), []string{"cp", "/host", "ctr:/ctr"}},
		{"apple copy in", (appleEngine{}).copyToArgs("ctr", "/host", "/ctr"), []string{"cp", "/host", "ctr:/ctr"}},
		{"docker copy out", (dockerEngine{}).copyFromArgs("ctr", "/ctr", "/host"), []string{"cp", "ctr:/ctr", "/host"}},
		{"docker exec", (dockerEngine{}).execArgs("ctr", execCfg, "/env", []string{"true"}), []string{"exec", "--env-file", "/env", "--user", "1000", "--workdir", "/work", "ctr", "true"}},
		{"apple exec", (appleEngine{}).execArgs("ctr", execCfg, "/env", []string{"true"}), []string{"exec", "--env-file", "/env", "--user", "1000", "--workdir", "/work", "ctr", "true"}},
		{"docker logs follow", (dockerEngine{}).logsFollowArgs("ctr"), []string{"logs", "--follow", "ctr"}},
		{"apple logs follow", (appleEngine{}).logsFollowArgs("ctr"), []string{"logs", "--follow", "ctr"}},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestBackendImageArgvKeepsSubcommandSpelling pins the parts of the pull and
// image-inspect argv that intentionally differ between the backends.
func TestBackendImageArgvKeepsSubcommandSpelling(t *testing.T) {
	if got, want := (dockerEngine{}).pullImageArgs("redis:7", "linux/amd64"), []string{"pull", "--platform", "linux/amd64", "redis:7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("docker pullImageArgs = %v, want %v", got, want)
	}
	if got, want := (appleEngine{}).pullImageArgs("redis:7", "linux/amd64"), []string{"image", "pull", "--platform", "linux/amd64", "redis:7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("apple pullImageArgs = %v, want %v", got, want)
	}
	if got, want := (dockerEngine{}).imageInspectArgs("redis:7", "linux/amd64"), []string{"image", "inspect", "--platform", "linux/amd64", "redis:7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("docker imageInspectArgs = %v, want %v", got, want)
	}
	if got, want := (appleEngine{}).imageInspectArgs("redis:7", "linux/amd64"), []string{"image", "inspect", "redis:7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("apple imageInspectArgs = %v, want %v", got, want)
	}
}

// wrappedAppleEngine models the test wrappers that embed a backend to
// override one method. Target-scope helpers must keep the wrapper when the
// backend reports no target scope.
type wrappedAppleEngine struct{ appleEngine }

// wrappedDockerEngine models a Docker wrapper. Target-scope helpers preserve it
// only while no explicit target setting is applied: the promoted target()
// returns the embedded dockerEngine, and configureTarget applies to that value.
type wrappedDockerEngine struct{ dockerEngine }

func TestTargetScopeHelpersKeepWrappedEngine(t *testing.T) {
	wrapped := wrappedAppleEngine{}

	host := "tcp://remote:2375"
	configured := configuredTarget(wrapped, engineTargetOverride{host: &host})
	if _, ok := configured.(wrappedAppleEngine); !ok {
		t.Fatalf("configuredTarget returned %T, want wrappedAppleEngine", configured)
	}

	frozen := frozenTarget(wrapped)
	if _, ok := frozen.(wrappedAppleEngine); !ok {
		t.Fatalf("frozenTarget returned %T, want wrappedAppleEngine", frozen)
	}

	resolved, err := resolvedTarget(context.Background(), wrapped, nil)
	if err != nil {
		t.Fatalf("resolvedTarget: %v", err)
	}
	if _, ok := resolved.(wrappedAppleEngine); !ok {
		t.Fatalf("resolvedTarget returned %T, want wrappedAppleEngine", resolved)
	}

	prepared, err := preparedPruneTarget(context.Background(), wrapped, nil, time.Second)
	if err != nil {
		t.Fatalf("preparedPruneTarget: %v", err)
	}
	if _, ok := prepared.(wrappedAppleEngine); !ok {
		t.Fatalf("preparedPruneTarget returned %T, want wrappedAppleEngine", prepared)
	}
}

func TestConfiguredTargetPreservesDockerWrapperWithoutSettings(t *testing.T) {
	wrapped := wrappedDockerEngine{}
	configured := configuredTarget(wrapped, engineTargetOverride{})
	if _, ok := configured.(wrappedDockerEngine); !ok {
		t.Fatalf("configuredTarget without settings returned %T, want wrappedDockerEngine", configured)
	}

	// An explicit setting reaches the embedded engine and replaces the wrapper;
	// configure the embedded engine before wrapping when overrides must persist.
	host := "tcp://remote:2375"
	configured = configuredTarget(wrapped, engineTargetOverride{host: &host})
	if _, ok := configured.(dockerEngine); !ok {
		t.Fatalf("configuredTarget with settings returned %T, want dockerEngine", configured)
	}
}

func TestConfiguredTargetAppliesDockerDaemonSettings(t *testing.T) {
	host := "tcp://remote:2375"
	contextName := "remote"
	configured, ok := configuredTarget(dockerEngine{}, engineTargetOverride{host: &host, context: &contextName}).(dockerEngine)
	if !ok {
		t.Fatalf("configuredTarget did not return dockerEngine")
	}
	if !configured.explicitHost || configured.host != host {
		t.Errorf("host = %q explicit = %v, want %q explicit", configured.host, configured.explicitHost, host)
	}
	if !configured.explicitContext || configured.context != contextName {
		t.Errorf("context = %q explicit = %v, want %q explicit", configured.context, configured.explicitContext, contextName)
	}
}

func TestFrozenTargetSnapshotsDockerEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://snapshot:2375")
	frozen, ok := frozenTarget(dockerEngine{}).(dockerEngine)
	if !ok {
		t.Fatalf("frozenTarget did not return dockerEngine")
	}
	if frozen.host != "tcp://snapshot:2375" {
		t.Errorf("frozen host = %q, want tcp://snapshot:2375", frozen.host)
	}
	if !frozen.explicitTarget {
		t.Error("frozen target did not mark the target explicit")
	}
}

func TestMaterializeRunPlatformUsesBackendDefault(t *testing.T) {
	apple := &config{eng: appleEngine{}}
	materializeRunPlatform(apple)
	if apple.platform != appleHostPlatform() {
		t.Errorf("apple platform = %q, want %q", apple.platform, appleHostPlatform())
	}

	explicit := &config{eng: appleEngine{}, platform: "linux/amd64"}
	materializeRunPlatform(explicit)
	if explicit.platform != "linux/amd64" {
		t.Errorf("explicit apple platform = %q, want linux/amd64", explicit.platform)
	}

	docker := &config{eng: dockerEngine{}}
	materializeRunPlatform(docker)
	if docker.platform != "" {
		t.Errorf("docker platform = %q, want empty", docker.platform)
	}
}
