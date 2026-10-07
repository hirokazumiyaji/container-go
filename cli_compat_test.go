package container

import (
	"os"
	"testing"
)

const appleLogsLiveEnv = "CONTAINERGO_APPLE_LIVE"

func appleLogsLiveEnabled() bool {
	return os.Getenv(appleLogsLiveEnv) == "1"
}

func TestAppleLogsLiveOptInGuard(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty", want: false},
		{name: "zero", value: "0", want: false},
		{name: "true word", value: "true", want: false},
		{name: "yes", value: "yes", want: false},
		{name: "padded", value: " 1", want: false},
		{name: "enabled", value: "1", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(appleLogsLiveEnv, tc.value)
			if got := appleLogsLiveEnabled(); got != tc.want {
				t.Fatalf("appleLogsLiveEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}
