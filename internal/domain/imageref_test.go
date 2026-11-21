package domain

import (
	"strings"
	"testing"
)

func TestNormalizeImageRef(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"redis", "docker.io/library/redis:latest"},
		{"redis:7", "docker.io/library/redis:7"},
		{"library/redis:7", "docker.io/library/redis:7"},
		{"docker.io/library/redis:7", "docker.io/library/redis:7"},
		{"ghcr.io/owner/repo:v1", "ghcr.io/owner/repo:v1"},
		{"localhost:5000/myimg", "localhost:5000/myimg:latest"},
	}

	for _, tt := range tests {
		got := NormalizeImageRef(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeImageRef(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestImagesCompatible(t *testing.T) {
	tests := []struct {
		req  string
		act  string
		want bool
	}{
		{"redis", "redis:latest", true},
		{"redis:7", "docker.io/library/redis:7", true},
		{"redis:7", "redis:8", false},
		{"redis@sha256:abc", "docker.io/library/redis@sha256:abc", true},
		{"redis@sha256:abc", "redis@sha256:def", false},
	}

	for _, tt := range tests {
		got := ImagesCompatible(tt.req, tt.act)
		if got != tt.want {
			t.Errorf("ImagesCompatible(%q, %q) = %v, want %v", tt.req, tt.act, got, tt.want)
		}
	}
}

func TestImageRepository(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"redis:7-alpine", "docker.io/library/redis"},
		{"docker.io/library/redis:7-alpine@sha256:123", "docker.io/library/redis"},
		{"ghcr.io/org/repo:tag", "ghcr.io/org/repo"},
	}

	for _, tt := range tests {
		got := ImageRepository(tt.input)
		if got != tt.want {
			t.Errorf("ImageRepository(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsHex(t *testing.T) {
	for _, s := range []string{"0", "abcdef", "ABCDEF0123456789"} {
		if !IsHex(s) {
			t.Errorf("IsHex(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "xyz", "0x12", "123g"} {
		if IsHex(s) {
			t.Errorf("IsHex(%q) = true, want false", s)
		}
	}
}

func TestParseImageRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		input  string
		name   string
		tag    string
		digest string
	}{
		{"redis", "redis", "", ""},
		{"redis:7", "redis", "7", ""},
		{"localhost:5000/myimg", "localhost:5000/myimg", "", ""},
		{"localhost:5000/myimg:v1", "localhost:5000/myimg", "v1", ""},
		{"redis:7@" + digest, "redis", "7", digest},
		{"redis@" + digest, "redis", "", digest},
	}
	for _, tt := range tests {
		parsed, err := ParseImageRef(tt.input)
		if err != nil {
			t.Errorf("ParseImageRef(%q): %v", tt.input, err)
			continue
		}
		if parsed.Name != tt.name || parsed.Tag != tt.tag || parsed.Digest != tt.digest {
			t.Errorf("ParseImageRef(%q) = %+v, want name=%q tag=%q digest=%q",
				tt.input, parsed, tt.name, tt.tag, tt.digest)
		}
	}
	if _, err := ParseImageRef(""); err == nil {
		t.Error("ParseImageRef(\"\") = nil error")
	}
}

func TestNormalizeImageRefPreservesDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		input string
		want  string
	}{
		{"redis@" + digest, "docker.io/library/redis:latest@" + digest},
		{"redis:7@" + digest, "docker.io/library/redis:7@" + digest},
		{"docker.io/library/redis:7@" + digest, "docker.io/library/redis:7@" + digest},
	}
	for _, tt := range tests {
		if got := NormalizeImageRef(tt.input); got != tt.want {
			t.Errorf("NormalizeImageRef(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestImagesCompatibleIgnoresTagForMatchingDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	if !ImagesCompatible("redis:7@"+digest, "redis@"+digest) {
		t.Error("the same digest with a redundant tag is not compatible")
	}
	if !ImagesCompatible("redis@"+digest, "docker.io/library/redis:7@"+digest) {
		t.Error("the same digest with equivalent repository spellings is not compatible")
	}
	other := "sha256:" + strings.Repeat("b", 64)
	if ImagesCompatible("redis:7@"+digest, "redis:8@"+other) {
		t.Error("different digests are compatible")
	}
	if ImagesCompatible("redis:7@"+digest, "postgres:7@"+digest) {
		t.Error("the same digest in a different repository is compatible")
	}
}

func TestIsBareImageReferenceRejectsUnprovenancedValues(t *testing.T) {
	for _, ref := range []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:latest",
		"sha256:abc",
		strings.Repeat("a", 64),
	} {
		if !IsBareImageReference(ref) {
			t.Errorf("IsBareImageReference(%q) = false, want true", ref)
		}
	}
	for _, ref := range []string{"redis:7", "docker.io/library/redis:7", "sha256:abc@sha256:def"} {
		if IsBareImageReference(ref) {
			t.Errorf("IsBareImageReference(%q) = true, want false", ref)
		}
	}
	if ImagesCompatible("sha256:not-a-valid-digest", "sha256:not-a-valid-digest") {
		t.Error("malformed digest-shaped references compare compatible")
	}
}
