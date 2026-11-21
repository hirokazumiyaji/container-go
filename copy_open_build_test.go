package container

import (
	"go/build"
	"testing"
)

// TestCopyOpenBuildConstraintsCoverSameHosts keeps the build constraints of
// the three openCopyFile implementations and copy_open_posix.go aligned. A
// POSIX host that builds copy_open_posix.go but none of the openCopyFile
// files fails to link, while overlapping providers fail to compile.
func TestCopyOpenBuildConstraintsCoverSameHosts(t *testing.T) {
	goosValues := []string{
		"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios",
		"js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows",
	}
	for _, goos := range goosValues {
		ctx := build.Default
		ctx.GOOS = goos
		posix := copyOpenFileBuilds(t, ctx, "copy_open_posix.go")
		unix := copyOpenFileBuilds(t, ctx, "copy_open_unix.go")
		other := copyOpenFileBuilds(t, ctx, "copy_open_other.go")
		windows := copyOpenFileBuilds(t, ctx, "copy_open_windows.go")

		if posix != unix {
			t.Errorf("GOOS=%s: copy_open_posix.go builds=%t, copy_open_unix.go builds=%t; want matching host sets", goos, posix, unix)
		}
		providers := 0
		for _, builds := range []bool{unix, other, windows} {
			if builds {
				providers++
			}
		}
		if providers != 1 {
			t.Errorf("GOOS=%s: openCopyFile has %d build providers, want exactly one", goos, providers)
		}
	}
}

func copyOpenFileBuilds(t *testing.T, ctx build.Context, name string) bool {
	t.Helper()
	builds, err := ctx.MatchFile(".", name)
	if err != nil {
		t.Fatalf("evaluate build constraints for %s: %v", name, err)
	}
	return builds
}
