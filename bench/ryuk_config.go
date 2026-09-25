package bench

import (
	"bufio"
	"fmt"
	"strings"
)

var testcontainersBenchmarkOverrides = map[string]bool{
	"TESTCONTAINERS_RYUK_DISABLED":         true,
	"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX": true,
	"TESTCONTAINERS_SESSION_ID":            true,
}

func validateTestcontainersEnvironmentOverride(name, value string) error {
	if !testcontainersBenchmarkOverrides[name] || value == "" {
		return nil
	}
	if name == "TESTCONTAINERS_RYUK_DISABLED" && strings.EqualFold(value, "false") {
		return nil
	}
	return fmt.Errorf("%s=%q overrides the pinned testcontainers benchmark identity", name, value)
}

func validateTestcontainersProperties(data string) error {
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			key, value, ok = strings.Cut(line, ":")
		}
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "ryuk.disabled":
			if value != "" && !strings.EqualFold(value, "false") {
				return fmt.Errorf(".testcontainers.properties ryuk.disabled=%q overrides the pinned benchmark identity", value)
			}
		case "hub.image.name.prefix", "session.id":
			if value != "" {
				return fmt.Errorf(".testcontainers.properties %s=%q overrides the pinned benchmark identity", key, value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read .testcontainers.properties: %w", err)
	}
	return nil
}

func validBenchmarkSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}
