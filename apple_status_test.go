package container

import (
	"encoding/json"
	"os"
	"testing"
)

// decodeAppleStatusVersion extracts the API-server version from the status
// JSON returned by Apple Container. Apple Container 1.2.2 exposes it at the
// top level, while newer layouts place it under server.version.
func decodeAppleStatusVersion(data []byte) (string, error) {
	var status struct {
		APIServerVersion string `json:"apiServerVersion"`
		Server           struct {
			Version string `json:"version"`
		} `json:"server"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return "", err
	}
	for _, raw := range []string{status.APIServerVersion, status.Server.Version} {
		if version := normalizeAppleVersion(raw); version != "" {
			return version, nil
		}
	}
	return "", nil
}

func TestAppleStatusVersionShapes(t *testing.T) {
	nested, err := os.ReadFile("testdata/apple_system_status_1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "1.3.0 nested server version",
			data: nested,
			want: "1.3.0",
		},
		{
			name: "legacy top-level version",
			data: []byte(`{"apiServerVersion":"1.2.2"}`),
			want: "1.2.2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeAppleStatusVersion(tc.data)
			if err != nil {
				t.Fatalf("decodeAppleStatusVersion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decodeAppleStatusVersion = %q, want %q", got, tc.want)
			}
		})
	}
}
