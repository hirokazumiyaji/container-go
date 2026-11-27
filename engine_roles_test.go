package container

import (
	"context"
	"errors"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// roleOmittingEngine exposes only the base engine contract of the wrapped
// backend. Embedding the base interface promotes exactly its methods, so the
// wrapped backend's optional roles are hidden. Tests use it to prove shared
// code handles a backend that omits a role the same way the former no-op did.
type roleOmittingEngine struct{ engine }

// newRoleOmittingContainer builds a verified Docker handle that omits every
// optional role.
func newRoleOmittingContainer(t *testing.T, f *fakeRunner) *Container {
	t.Helper()
	return &Container{
		id:       "myctr",
		runner:   f,
		eng:      roleOmittingEngine{dockerEngine{}},
		creation: "0123456789abcdef",
		sync:     &lockPack{uid: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
	}
}

func TestPauseAndUnpauseWithoutLifecycleRoleAreUnsupported(t *testing.T) {
	f := newDockerTestRunner()
	ctr := newRoleOmittingContainer(t, f)

	if err := ctr.Pause(context.Background()); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("Pause error = %v, want ErrUnsupportedCapability", err)
	}
	if call := f.callWith("pause"); call != nil {
		t.Fatalf("Pause without the lifecycle role invoked the CLI: %v", call)
	}
	if err := ctr.Unpause(context.Background()); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("Unpause error = %v, want ErrUnsupportedCapability", err)
	}
	if call := f.callWith("unpause"); call != nil {
		t.Fatalf("Unpause without the lifecycle role invoked the CLI: %v", call)
	}
}

func TestRestartWithoutLifecycleRoleFallsBackToStopAndStart(t *testing.T) {
	f := newDockerTestRunner()
	ctr := newRoleOmittingContainer(t, f)

	if err := ctr.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart error = %v", err)
	}
	if call := f.callWith("restart"); call != nil {
		t.Fatalf("Restart without the lifecycle role used the native command: %v", call)
	}
	if call := f.callWith("stop"); call == nil {
		t.Fatal("Restart fallback did not stop the container")
	}
	if call := f.callWith("start"); call == nil {
		t.Fatal("Restart fallback did not start the container")
	}
}

func TestCopyFileFromContainerWithoutCopyOutRoleIsUnsupported(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	ctr := &Container{id: "myctr", runner: f, eng: roleOmittingEngine{dockerEngine{}}}

	_, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}
	if call := f.callWith("cp"); call != nil {
		t.Fatalf("copy-out without the role invoked the CLI: %v", call)
	}
}

func TestReuseNetworkCompatWithoutRoleIsSkipped(t *testing.T) {
	cfg := newConfig()
	cfg.eng = roleOmittingEngine{dockerEngine{}}
	cfg.name = "myctr"
	cfg.network = "test-net"
	cfg.networkExplicit = true
	info := &engineInfo{
		networkMode: dockerNetworkBridge,
		labels:      map[string]string{domain.LabelReuseConfig: cfg.reuseConfigHash()},
	}
	if err := checkReuseConfigCompat(info, cfg); err != nil {
		t.Fatalf("checkReuseConfigCompat without engineNetworkCompat = %v, want nil", err)
	}

	cfg.eng = dockerEngine{}
	err := checkReuseConfigCompat(info, cfg)
	if !errors.Is(err, ErrReuseMismatch) {
		t.Fatalf("checkReuseConfigCompat with engineNetworkCompat = %v, want ErrReuseMismatch", err)
	}
}
