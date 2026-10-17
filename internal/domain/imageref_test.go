package domain

import (
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

func TestSplitImageTagStripsDigest(t *testing.T) {
	tests := []struct {
		input    string
		wantBase string
		wantTag  string
	}{
		{"redis:7", "redis", "7"},
		{"alpine@sha256:abc", "alpine", ""},
		{"alpine:3@sha256:abc", "alpine", "3"},
		{"registry:5000/team/app:v1", "registry:5000/team/app", "v1"},
	}

	for _, tt := range tests {
		base, tag := SplitImageTag(tt.input)
		if base != tt.wantBase || tag != tt.wantTag {
			t.Errorf("SplitImageTag(%q) = (%q, %q), want (%q, %q)", tt.input, base, tag, tt.wantBase, tt.wantTag)
		}
	}
}
