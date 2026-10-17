package container

import (
	"maps"
	"testing"
)

// allLabels must carry every internal label exactly once and keep
// caller-supplied labels from overriding them. The assignment count is
// tested in internal/domain, where BuildLabels owns it.
func TestAllLabelsSetsEachInternalLabelOnce(t *testing.T) {
	cfg := newConfig()
	cfg.creation = "abc123"
	cfg.reuse = true
	cfg.reuseGroup = "grp"
	cfg.labels = map[string]string{"user": "value", managedLabel: "false"}

	got := cfg.allLabels()
	want := map[string]string{
		"user":          "value",
		managedLabel:    "true",
		sessionLabel:    sessionID(),
		creationLabel:   "abc123",
		reuseLabel:      "true",
		reuseGroupLabel: "grp",
	}
	if !maps.Equal(got, want) {
		t.Errorf("allLabels() = %v, want %v", got, want)
	}
}

// A config that activates no optional label must still carry the two
// labels that are unconditional, and no empty-valued ones.
func TestAllLabelsOmitsUnsetOptionalLabels(t *testing.T) {
	got := newConfig().allLabels()
	want := map[string]string{
		managedLabel: "true",
		sessionLabel: sessionID(),
	}
	if !maps.Equal(got, want) {
		t.Errorf("allLabels() = %v, want %v", got, want)
	}
}

// Internal labels win over a reserved key that reached config without
// going through WithLabels. WithLabels rejects those keys, so this
// state is only reachable by constructing a config directly - the
// overwrite is what keeps the generation label trustworthy if that
// ever stops being the only path.
func TestAllLabelsInternalLabelsWinOverReserved(t *testing.T) {
	cfg := newConfig()
	cfg.creation = "real"
	cfg.reuse = true
	cfg.reuseGroup = "realgroup"
	cfg.labels = map[string]string{
		managedLabel:    "false",
		sessionLabel:    "forged",
		creationLabel:   "forged",
		reuseLabel:      "false",
		reuseGroupLabel: "forged",
	}

	got := cfg.allLabels()
	want := map[string]string{
		managedLabel:    "true",
		sessionLabel:    sessionID(),
		creationLabel:   "real",
		reuseLabel:      "true",
		reuseGroupLabel: "realgroup",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
}

// A forged optional reuse label must not survive when the config did not
// request reuse: otherwise an ownership check could accept a container
// that was never created through the reuse path.
func TestAllLabelsDropsForgedOptionalReservedLabels(t *testing.T) {
	cfg := newConfig()
	cfg.labels = map[string]string{
		managedLabel:    "false",
		sessionLabel:    "forged",
		creationLabel:   "forged",
		reuseLabel:      "true",
		reuseGroupLabel: "forged",
	}

	got := cfg.allLabels()
	want := map[string]string{
		managedLabel: "true",
		sessionLabel: sessionID(),
	}
	if !maps.Equal(got, want) {
		t.Errorf("allLabels() = %v, want %v", got, want)
	}
}
