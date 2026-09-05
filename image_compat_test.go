package container

import (
	"context"
	"strings"
	"testing"
)

func TestImagesCompatibleRejectsForeignRegistry(t *testing.T) {
	cases := []struct {
		req, act string
	}{
		{"redis:7-alpine", "private.example/team/redis:7-alpine"},
		{"redis:7-alpine", "evil.example/library/redis:7-alpine"},
		{"myorg/app:v1", "otherorg/app:v1"},
		{"redis:7-alpine", "redis:8-alpine"},
		{"redis:7-alpine", "library/redis:8-alpine"},
		{"docker.io/library/redis:7-alpine", "private.example/library/redis:7-alpine"},
	}
	for _, tc := range cases {
		if imagesCompatible(tc.req, tc.act) {
			t.Errorf("imagesCompatible(%q,%q) = true, want false", tc.req, tc.act)
		}
	}
}

func TestImagesCompatibleAcceptsHubShortForms(t *testing.T) {
	cases := []struct {
		req, act string
	}{
		{"redis", "docker.io/library/redis:latest"},
		{"redis:7-alpine", "docker.io/library/redis:7-alpine"},
		{"library/redis:7-alpine", "docker.io/library/redis:7-alpine"},
		{"docker.io/library/redis:7-alpine", "redis:7-alpine"},
		{"redis:7-alpine", "redis:7-alpine@sha256:abc"},
	}
	for _, tc := range cases {
		if !imagesCompatible(tc.req, tc.act) {
			t.Errorf("imagesCompatible(%q,%q) = false, want true", tc.req, tc.act)
		}
	}
}

func TestImagesCompatibleDigestRequiresBaseMatch(t *testing.T) {
	if imagesCompatible("redis:7-alpine@sha256:abc", "evil.example/redis:7-alpine@sha256:abc") {
		t.Error("different registry with same digest must not match")
	}
	if !imagesCompatible("redis:7-alpine@sha256:abc", "docker.io/library/redis:7-alpine@sha256:abc") {
		t.Error("same base and digest must match")
	}
}

func TestReuseStoppedMismatchDoesNotDelete(t *testing.T) {
	f := &stoppedMismatchRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want image mismatch", err)
	}
	if f.deleted {
		t.Error("incompatible stopped container was deleted")
	}
}

type stoppedMismatchRunner struct {
	*fakeRunner
	deleted bool
}

func (s *stoppedMismatchRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	s.mu.Unlock()
	if args[0] == "image" {
		return s.fakeRunner.Run(ctx, args...)
	}
	switch args[0] {
	case "inspect":
		return []byte(reuseInspectJSON(args[len(args)-1], "stopped", "nginx:alpine")), nil, nil
	case "delete", "rm":
		s.deleted = true
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}
