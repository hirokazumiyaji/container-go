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
		{name: "architecture selector", selector: "linux/arm64", actual: "linux", want: true},
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
