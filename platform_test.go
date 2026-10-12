package container

import "testing"

func TestDockerPlatformCompatible(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		actual   string
		want     bool
	}{
		{name: "OS-only actual", selector: "linux", actual: "linux", want: true},
		{name: "architecture selector unverifiable", selector: "linux/arm64", actual: "linux"},
		{name: "OS selector", selector: "linux", actual: "linux/amd64", want: true},
		{name: "reported architecture matches", selector: "linux/arm64", actual: "linux/arm64", want: true},
		{name: "reported architecture differs", selector: "linux/arm64", actual: "linux/amd64"},
		{name: "reported variant differs", selector: "linux/arm/v7", actual: "linux/arm/v6"},
		{name: "OS differs", selector: "windows/arm64", actual: "linux"},
		{name: "actual missing", selector: "linux", actual: ""},
		{name: "actual malformed", selector: "linux", actual: "linux/amd64/unknown/extra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (dockerEngine{}).platformCompatible(tc.selector, tc.actual); got != tc.want {
				t.Fatalf("platformCompatible(%q, %q) = %v, want %v", tc.selector, tc.actual, got, tc.want)
			}
		})
	}
}

func TestPlatformSelectorChecksOnlyNamedComponents(t *testing.T) {
	reported := platformMetadataFromParts("linux", "arm64", "", true, true, true)
	if actual := reported.normalized(); actual != "linux/arm64/" {
		t.Fatalf("normalized platform = %q", actual)
	}
	if !platformMetadataMatches("linux/arm64", reported) {
		t.Fatal("empty unreported variant invalidated an architecture-only selector")
	}
	if platformSelectorUnverifiable("linux/arm64", reported.normalized()) {
		t.Fatal("architecture-only selector was reported as unverifiable")
	}
	if platformMetadataMatches("linux/arm/v7", reported) {
		t.Fatal("empty selected variant was treated as a match")
	}
	if platformMetadataMatches("darwin/arm64", reported) {
		t.Fatal("different selected OS was treated as a match")
	}
}

func TestApplePlatformSelectorIsArchitectureAware(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		actual   string
		want     bool
	}{
		{name: "OS selector is unconstrained", selector: "linux", actual: "linux/arm64", want: true},
		{name: "architecture matches", selector: "linux/arm64", actual: "linux/arm64", want: true},
		{name: "architecture differs", selector: "linux/amd64", actual: "linux/arm64"},
		{name: "variant is pinned", selector: "linux/arm/v7", actual: "linux/arm"},
		{name: "OS differs", selector: "darwin/arm64", actual: "linux/arm64"},
		{name: "actual missing", selector: "linux", actual: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (appleEngine{}).platformCompatible(tc.selector, tc.actual); got != tc.want {
				t.Fatalf("platformCompatible(%q, %q) = %v, want %v", tc.selector, tc.actual, got, tc.want)
			}
		})
	}
}
