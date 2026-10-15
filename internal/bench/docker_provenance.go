package bench

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
)

// rejectDockerConnectionOverrides prevents a benchmark from silently using
// a different TLS, API, or authentication configuration than the recorded
// endpoint and daemon. These settings are either empty/canonical or the
// benchmark stops before measurement.
func rejectDockerConnectionOverrides() error {
	if value := os.Getenv("DOCKER_TLS_VERIFY"); value != "" && value != "0" {
		return fmt.Errorf("DOCKER_TLS_VERIFY overrides the canonical Docker benchmark connection")
	}
	for _, name := range []string{"DOCKER_CERT_PATH", "DOCKER_API_VERSION", "DOCKER_AUTH_CONFIG", "DOCKER_CONFIG"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s overrides the canonical Docker benchmark connection", name)
		}
	}
	return nil
}

// captureDockerProvenance records the endpoint and daemon selected by the
// Docker CLI, not merely the client version. It fails when context and
// DOCKER_HOST disagree because the benchmark would otherwise mix daemons
// while presenting one Docker result key.
func captureDockerProvenance(run func(...string) ([]byte, error)) (BackendProvenance, error) {
	if err := rejectDockerConnectionOverrides(); err != nil {
		return BackendProvenance{}, err
	}
	infoOut, err := run("info", "--format", "{{json .}}")
	if err != nil {
		return BackendProvenance{}, fmt.Errorf("record Docker daemon provenance: %w", err)
	}
	var info struct {
		ID              string `json:"ID"`
		Name            string `json:"Name"`
		OperatingSystem string `json:"OperatingSystem"`
		OSType          string `json:"OSType"`
		Architecture    string `json:"Architecture"`
	}
	if err := json.Unmarshal(infoOut, &info); err != nil {
		return BackendProvenance{}, fmt.Errorf("decode Docker daemon provenance: %w", err)
	}
	daemonID := strings.TrimSpace(info.ID)
	if daemonID == "" {
		return BackendProvenance{}, fmt.Errorf("docker daemon provenance has no ID")
	}
	daemonOS := strings.TrimSpace(info.OSType)
	if daemonOS == "" {
		daemonOS = strings.TrimSpace(info.OperatingSystem)
	}
	daemonArch := strings.TrimSpace(info.Architecture)
	if daemonOS == "" || daemonArch == "" {
		return BackendProvenance{}, fmt.Errorf("docker daemon provenance has incomplete OS/architecture: OS=%q arch=%q", daemonOS, daemonArch)
	}

	contextOut, err := run("context", "show")
	if err != nil {
		return BackendProvenance{}, fmt.Errorf("record Docker context: %w", err)
	}
	contextName := strings.TrimSpace(string(contextOut))
	if contextName == "" {
		return BackendProvenance{}, fmt.Errorf("docker context is empty")
	}
	if raw := os.Getenv("DOCKER_CONTEXT"); raw != strings.TrimSpace(raw) {
		return BackendProvenance{}, fmt.Errorf("DOCKER_CONTEXT must not contain surrounding whitespace")
	} else if configured := strings.TrimSpace(raw); configured != "" && configured != contextName {
		return BackendProvenance{}, fmt.Errorf("DOCKER_CONTEXT=%q but Docker reports context %q", configured, contextName)
	}

	contextOut, err = run("context", "inspect", contextName)
	if err != nil {
		return BackendProvenance{}, fmt.Errorf("inspect Docker context %q: %w", contextName, err)
	}
	contextEndpoint, err := dockerContextEndpoint(contextOut, contextName)
	if err != nil {
		return BackendProvenance{}, err
	}
	effectiveEndpoint := contextEndpoint
	if raw := os.Getenv("DOCKER_HOST"); raw != strings.TrimSpace(raw) {
		return BackendProvenance{}, fmt.Errorf("DOCKER_HOST must not contain surrounding whitespace")
	} else if configured := strings.TrimSpace(raw); configured != "" {
		if err := validateDockerEndpoint(configured); err != nil {
			return BackendProvenance{}, err
		}
		if contextEndpoint != "" && normalizeDockerEndpoint(configured) != normalizeDockerEndpoint(contextEndpoint) {
			return BackendProvenance{}, fmt.Errorf("DOCKER_HOST endpoint %q disagrees with context %q endpoint %q", configured, contextName, contextEndpoint)
		}
		effectiveEndpoint = configured
	}
	if effectiveEndpoint == "" {
		return BackendProvenance{}, fmt.Errorf("docker context %q has no effective endpoint", contextName)
	}
	if err := validateDockerEndpoint(effectiveEndpoint); err != nil {
		return BackendProvenance{}, err
	}
	return BackendProvenance{
		Endpoint:   normalizeDockerEndpoint(effectiveEndpoint),
		Context:    contextName,
		DaemonID:   daemonID,
		DaemonOS:   daemonOS,
		DaemonArch: daemonArch,
	}, nil
}

func dockerContextEndpoint(data []byte, name string) (string, error) {
	var contexts []struct {
		Name      string `json:"Name"`
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &contexts); err != nil {
		return "", fmt.Errorf("decode Docker context %q: %w", name, err)
	}
	var matches []string
	for _, context := range contexts {
		if context.Name != name {
			continue
		}
		if endpoint, ok := context.Endpoints["docker"]; ok && strings.TrimSpace(endpoint.Host) != "" {
			matches = append(matches, strings.TrimSpace(endpoint.Host))
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("docker context %q returned %d docker endpoints", name, len(matches))
	}
	return matches[0], nil
}

func validateDockerEndpoint(endpoint string) error {
	if endpoint != strings.TrimSpace(endpoint) {
		return fmt.Errorf("docker endpoint %q contains surrounding whitespace", endpoint)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" {
		return fmt.Errorf("invalid Docker endpoint %q", endpoint)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("docker endpoint %q contains credentials or query parameters", endpoint)
	}
	return nil
}

func normalizeDockerEndpoint(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.Path != "" {
		parsed.Path = path.Clean(parsed.Path)
	}
	return parsed.String()
}
