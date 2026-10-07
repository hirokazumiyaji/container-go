//go:build windows

package container

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestWindowsBindMountUsesHostPathAndPOSIXTarget(t *testing.T) {
	const source = `C:\tmp\data`
	mount := Mount{Type: MountBind, Source: source, Target: "/data", ReadOnly: true}
	if err := mount.validate(); err != nil {
		t.Fatalf("validate Windows bind mount: %v", err)
	}

	windowsTarget := Mount{Type: MountBind, Source: source, Target: `C:\data`}
	if err := windowsTarget.validate(); err == nil {
		t.Fatal("Windows path used as container target was accepted")
	}

	t.Setenv("DOCKER_HOST", "")
	cfg := dockerTestConfig(t, WithMounts(mount))
	if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("local Windows bind mount: %v", err)
	}
	args := (dockerEngine{}).runArgs(cfg, "redis:7-alpine", "")
	want := "type=bind,source=" + source + ",target=/data,readonly"
	i := slices.Index(args, "--mount")
	if i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Errorf("mount argv = %v, want --mount %q", args, want)
	}
}

func TestWindowsBindMountRejectsRemoteDocker(t *testing.T) {
	t.Setenv(backendEnv, "docker")
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f),
		WithMounts(Mount{Type: MountBind, Source: `C:\tmp\data`, Target: "/data"}))
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("remote bind mount must fail before backend work: %v", f.calls)
	}
}
