package container

import (
	"os"
	"testing"
)

func TestAppleInspectCarriesOCIImageDigest(t *testing.T) {
	data, err := os.ReadFile("internal/inspect/testdata/inspect_v1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	info, err := (appleEngine{}).parseInspect(data, "containergo-1a2b3c4d")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	wantDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if info.imageDigest != wantDigest {
		t.Fatalf("imageDigest = %q, want %q", info.imageDigest, wantDigest)
	}
	if info.image != "docker.io/library/redis:7-alpine@"+wantDigest {
		t.Fatalf("image = %q, want digest-qualified reference", info.image)
	}
}

func TestPlatformSelectorMatchesIndividualFields(t *testing.T) {
	cases := []struct {
		requested string
		actual    string
		want      bool
	}{
		{"linux", "linux/amd64", true},
		{"linux/amd64", "linux/amd64/v3", true},
		{"linux/arm64/v8", "linux/arm64/v8", true},
		{"linux/arm64", "linux/amd64", false},
		{"linux/amd64/v3", "linux/amd64", false},
		{"linux/amd64", "", false},
	}
	for _, tc := range cases {
		if got := platformSelectorMatches(tc.requested, tc.actual); got != tc.want {
			t.Errorf("platformSelectorMatches(%q, %q) = %v, want %v", tc.requested, tc.actual, got, tc.want)
		}
	}
}

func TestReuseCarriesAppleDescriptorDigestIntoCompatibility(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	info := &engineInfo{
		image:       "redis:7-alpine@" + digest,
		imageDigest: digest,
		platform:    "linux/arm64",
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "0123456789abcdef",
		},
	}
	cfg := &config{name: "shared", platform: "linux"}
	if err := checkReuseOwned(info, "redis:7-alpine@"+digest, cfg); err != nil {
		t.Fatalf("digest-qualified reuse mismatch: %v", err)
	}
	if !imagesCompatible("redis:7-alpine@"+digest, info.image) {
		t.Fatal("digest-qualified image should compare equal")
	}
}
