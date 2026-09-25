package container

import (
	"strings"
	"testing"
)

// normalizeAppleVersion accepts both the CLI's bare semantic version and the
// API server's full single-line ReleaseVersion form.
func normalizeAppleVersion(raw string) string {
	fields := strings.Fields(raw)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "version" {
			return fields[i+1]
		}
	}
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func TestNormalizeAppleVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "CLI version", input: "1.3.0", want: "1.3.0"},
		{
			name:  "API server version with build",
			input: "container-apiserver version 1.3.0 (build: release, commit: abc1234)",
			want:  "1.3.0",
		},
		{
			name:  "API server status fallback",
			input: "  container-apiserver version 1.2.2 (build: debug, commit: def5678)  ",
			want:  "1.2.2",
		},
		{name: "empty", input: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAppleVersion(tc.input); got != tc.want {
				t.Errorf("normalizeAppleVersion(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
