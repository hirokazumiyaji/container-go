//go:build windows

package container

import (
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

	cfg := dockerTestConfig(t, WithMounts(mount))
	args := (dockerEngine{}).runArgs(cfg, "redis:7-alpine", "")
	want := "type=bind,source=" + source + ",target=/data,readonly"
	i := slices.Index(args, "--mount")
	if i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Errorf("mount argv = %v, want --mount %q", args, want)
	}
}
