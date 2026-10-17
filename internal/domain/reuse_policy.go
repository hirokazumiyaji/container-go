package domain

import (
	"fmt"
)

// CheckReuseLabels checks whether an inspected container's labels satisfy reuse ownership requirements.
func CheckReuseLabels(name string, labels map[string]string) error {
	if labels == nil {
		return fmt.Errorf("reuse %s: inspect returned no container identity", name)
	}
	if labels[LabelManaged] != "true" {
		return fmt.Errorf("reuse %s: existing container is not managed by container-go", name)
	}
	if labels[LabelReuse] != "true" {
		return fmt.Errorf("reuse: existing container was not created with WithReuse")
	}
	return nil
}

// CheckReuseGroup verifies that the container's reuse group matches the expected group.
func CheckReuseGroup(name string, labels map[string]string, expectedGroup string) error {
	if expectedGroup == "" {
		return nil
	}
	actualGroup := labels[LabelReuseGroup]
	if actualGroup != expectedGroup {
		return fmt.Errorf("reuse %s: reuse group mismatch: want %q, got %q", name, expectedGroup, actualGroup)
	}
	return nil
}
