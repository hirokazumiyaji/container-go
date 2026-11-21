package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileFromContentNormalizesNilContent(t *testing.T) {
	f := FileFromContent("/app/empty.txt", nil)
	if f.Content == nil {
		t.Fatal("FileFromContent(nil) stored a nil Content; an empty file must remain distinguishable from an unset source")
	}
	if len(f.Content) != 0 {
		t.Fatalf("Content = %q, want empty", f.Content)
	}
	if f.HostPath != "" {
		t.Fatalf("HostPath = %q, want empty", f.HostPath)
	}
	if f.ContainerPath != "/app/empty.txt" {
		t.Fatalf("ContainerPath = %q", f.ContainerPath)
	}
	if f.Mode != 0 {
		t.Fatalf("Mode = %#o, want the zero default", f.Mode)
	}

	cfg := newConfig()
	if err := WithFiles(f)(cfg); err != nil {
		t.Fatalf("WithFiles(FileFromContent(nil)): %v", err)
	}
	if len(cfg.files) != 1 || cfg.files[0].Content == nil {
		t.Fatalf("validated files = %+v, want one empty-content file", cfg.files)
	}
}

func TestFileFromContentKeepsContentAndMode(t *testing.T) {
	f := FileFromContent("/etc/secret", []byte("value"), 0o640)
	if string(f.Content) != "value" {
		t.Fatalf("Content = %q, want value", f.Content)
	}
	if f.Mode != 0o640 {
		t.Fatalf("Mode = %#o, want 0640", f.Mode)
	}
	if err := WithFiles(f)(newConfig()); err != nil {
		t.Fatalf("WithFiles: %v", err)
	}
}

func TestFileFromHostResolvesHostPathOnApply(t *testing.T) {
	source := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(source, []byte("key=value"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := FileFromHost(source, "/etc/config.txt")
	if f.Content != nil {
		t.Fatalf("HostPath file has Content %q, want nil", f.Content)
	}
	if f.Mode != 0 {
		t.Fatalf("Mode = %#o, want the zero default", f.Mode)
	}

	cfg := newConfig()
	if err := WithFiles(f)(cfg); err != nil {
		t.Fatalf("WithFiles(FileFromHost): %v", err)
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.files[0].HostPath != abs {
		t.Fatalf("resolved host path = %q, want %q", cfg.files[0].HostPath, abs)
	}
}

func TestFileFromHostKeepsMode(t *testing.T) {
	source := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(source, []byte("key=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := FileFromHost(source, "/etc/config.txt", 0o644)
	if f.Mode != 0o644 {
		t.Fatalf("Mode = %#o, want 0644", f.Mode)
	}
}

// The constructors shape a valid source but do not skip option validation.
func TestFileConstructorsStillValidate(t *testing.T) {
	if err := WithFiles(FileFromContent("relative", nil))(newConfig()); err == nil {
		t.Fatal("FileFromContent accepted a relative container path")
	}
	if err := WithFiles(FileFromContent("/ok", make([]byte, int(MaxCopyToContainerSize)+1)))(newConfig()); err == nil {
		t.Fatal("FileFromContent accepted content above MaxCopyToContainerSize")
	}
	missing := filepath.Join(t.TempDir(), "missing.txt")
	if err := WithFiles(FileFromHost(missing, "/etc/missing.txt"))(newConfig()); err == nil {
		t.Fatal("FileFromHost accepted a missing host path")
	}
}

func TestMountConstructors(t *testing.T) {
	hostSource := t.TempDir()
	tests := []struct {
		name  string
		mount Mount
		want  Mount
		arg   string
	}{
		{
			name:  "bind",
			mount: BindMount(hostSource, "/data"),
			want:  Mount{Type: MountBind, Source: hostSource, Target: "/data"},
			arg:   "type=bind,source=" + hostSource + ",target=/data",
		},
		{
			name:  "bind read-only",
			mount: BindMount(hostSource, "/data", true),
			want:  Mount{Type: MountBind, Source: hostSource, Target: "/data", ReadOnly: true},
			arg:   "type=bind,source=" + hostSource + ",target=/data,readonly",
		},
		{
			name:  "bind writable",
			mount: BindMount(hostSource, "/data", false),
			want:  Mount{Type: MountBind, Source: hostSource, Target: "/data"},
			arg:   "type=bind,source=" + hostSource + ",target=/data",
		},
		{
			name:  "volume",
			mount: VolumeMount("cache", "/cache"),
			want:  Mount{Type: MountVolume, Source: "cache", Target: "/cache"},
			arg:   "type=volume,source=cache,target=/cache",
		},
		{
			name:  "volume read-only",
			mount: VolumeMount("cache", "/cache", true),
			want:  Mount{Type: MountVolume, Source: "cache", Target: "/cache", ReadOnly: true},
			arg:   "type=volume,source=cache,target=/cache,readonly",
		},
		{
			name:  "tmpfs",
			mount: TmpfsMount("/tmp"),
			want:  Mount{Type: MountTmpfs, Target: "/tmp"},
			arg:   "type=tmpfs,target=/tmp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mount != tt.want {
				t.Fatalf("mount = %+v, want %+v", tt.mount, tt.want)
			}
			if err := tt.mount.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := tt.mount.arg(); got != tt.arg {
				t.Fatalf("arg = %q, want %q", got, tt.arg)
			}
			cfg := newConfig()
			if err := WithMounts(tt.mount)(cfg); err != nil {
				t.Fatalf("WithMounts: %v", err)
			}
			if len(cfg.mounts) != 1 || cfg.mounts[0] != tt.want {
				t.Fatalf("configured mounts = %+v", cfg.mounts)
			}
		})
	}
}

// The smart constructors cannot express the invalid combinations the raw
// struct still can; option validation remains the backstop for those.
func TestMountConstructorsAvoidInvalidCombinations(t *testing.T) {
	if m := TmpfsMount("/tmp"); m.Source != "" {
		t.Fatalf("TmpfsMount source = %q, want empty", m.Source)
	}
	if m := BindMount("/host", "/data"); m.Type != MountBind || m.Source != "/host" {
		t.Fatalf("BindMount = %+v", m)
	}
	if m := VolumeMount("cache", "/cache"); m.Type != MountVolume || m.Source != "cache" {
		t.Fatalf("VolumeMount = %+v", m)
	}

	// The source/target arguments still reach the shared validation.
	if err := BindMount("relative/path", "/data").validate(); err == nil {
		t.Fatal("BindMount accepted a relative host source")
	}
	if err := (Mount{Type: MountTmpfs, Source: "/host", Target: "/tmp"}).validate(); err == nil {
		t.Fatal("raw tmpfs mount with a source was accepted")
	}
}
