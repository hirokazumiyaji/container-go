package container

import (
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

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

func TestPlatformSelectorUnverifiableOnlyForUnreadableMetadata(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		actual   string
		want     bool
	}{
		{name: "matching platform", selector: "linux/arm64", actual: "linux/arm64"},
		{name: "different architecture", selector: "linux/arm64", actual: "linux/amd64"},
		{name: "different OS", selector: "windows/amd64", actual: "linux/amd64"},
		{name: "unreported architecture", selector: "linux/arm64", actual: "linux"},
		{name: "malformed actual", selector: "linux/arm64", actual: "linux/arm64/v8/extra", want: true},
		{name: "malformed selector", selector: "linux//arm64", actual: "linux/arm64", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformSelectorUnverifiable(tc.selector, tc.actual); got != tc.want {
				t.Fatalf("platformSelectorUnverifiable(%q, %q) = %v, want %v", tc.selector, tc.actual, got, tc.want)
			}
		})
	}
}

func TestReusePlatformDiagnosisDistinguishesMismatch(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		wantReason string
	}{
		{name: "matching platform", platform: "linux/amd64"},
		{name: "verified mismatch", platform: "linux/arm64", wantReason: "does not match existing"},
		{name: "unreported platform", wantReason: "could not be verified"},
		{name: "malformed platform", platform: "linux/arm64/v8/extra", wantReason: "could not be verified"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			cfg.eng = appleEngine{}
			cfg.name = "myctr"
			if err := WithPlatform("linux/amd64")(cfg); err != nil {
				t.Fatal(err)
			}
			info := &engineInfo{
				platform:     tc.platform,
				platformMeta: platformMetadataFromString(tc.platform),
				labels:       map[string]string{domain.LabelReuseConfig: cfg.reuseConfigHash()},
			}
			err := checkReuseConfigCompat(info, cfg)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("compatible platform rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrReuseMismatch) {
				t.Fatalf("error = %v, want ErrReuseMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("error = %v, want %q", err, tc.wantReason)
			}
		})
	}
}
