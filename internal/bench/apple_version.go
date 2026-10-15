package bench

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseAppleServiceVersion extracts only the API server component from
// `container system version --format json`. Recording the whole JSON would
// make a client-only response look like a service version and would retain
// fields that are not comparable across Apple Container builds.
func parseAppleServiceVersion(data []byte) (string, error) {
	var components []struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &components); err != nil {
		return "", fmt.Errorf("decode Apple system version JSON: %w", err)
	}
	if components == nil {
		return "", fmt.Errorf("decode Apple system version JSON: expected an array, got null")
	}
	versions := make([]string, 0, 1)
	for _, component := range components {
		if component.AppName != "container-apiserver" {
			continue
		}
		version := strings.TrimSpace(component.Version)
		if version == "" || strings.ContainsAny(version, "\r\n") {
			return "", fmt.Errorf("apple system version component %q has an invalid version", component.AppName)
		}
		versions = append(versions, version)
	}
	if len(versions) != 1 {
		return "", fmt.Errorf("apple system version JSON contains %d container-apiserver components, want exactly one", len(versions))
	}
	return versions[0], nil
}
