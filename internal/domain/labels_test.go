package domain

import (
	"testing"
)

func TestBuildLabels(t *testing.T) {
	cfg := LabelConfig{
		Session:  "sess-123",
		Creation: "gen-456",
		Reuse:    true,
		Custom: map[string]string{
			"app": "web",
		},
	}

	labels := BuildLabels(cfg)
	if labels[LabelManaged] != "true" {
		t.Errorf("LabelManaged = %q, want true", labels[LabelManaged])
	}
	if labels[LabelSession] != "sess-123" {
		t.Errorf("LabelSession = %q, want sess-123", labels[LabelSession])
	}
	if labels[LabelCreation] != "gen-456" {
		t.Errorf("LabelCreation = %q, want gen-456", labels[LabelCreation])
	}
	if labels[LabelReuse] != "true" {
		t.Errorf("LabelReuse = %q, want true", labels[LabelReuse])
	}
	if labels["app"] != "web" {
		t.Errorf("custom label app = %q, want web", labels["app"])
	}
}
