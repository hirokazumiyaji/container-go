package bench

import (
	"encoding/json"
	"fmt"
)

// BaselineScenario records every policy input represented by a baseline row.
// Keeping the complete identity and state lists here prevents a documentation
// check from validating only the row count and iteration column.
type BaselineScenario struct {
	Backend             string   `json:"backend"`
	Library             string   `json:"library"`
	Scenario            string   `json:"scenario"`
	Image               string   `json:"image"`
	ImageDigest         string   `json:"image_digest"`
	WorkloadCacheStates []string `json:"workload_cache_states"`
	RyukCacheStates     []string `json:"ryuk_cache_states,omitempty"`
	Iterations          int      `json:"iterations"`
}

// BaselineProvenance is the complete, machine-checkable description of a
// benchmark baseline. It is intentionally separate from measured durations:
// the table may contain pending or historical values, while this record fixes
// the source, environment, and scenario inputs those values were allowed to
// represent.
type BaselineProvenance struct {
	SchemaVersion int                `json:"schema_version"`
	Commit        string             `json:"commit"`
	Tree          string             `json:"tree"`
	Dirty         bool               `json:"dirty"`
	Env           Env                `json:"env"`
	Scenarios     []BaselineScenario `json:"scenarios"`

	metadataPresent bool
	dirtyPresent    bool
}

// UnmarshalJSON preserves whether the top-level dirty marker was present.
// A false value and an omitted value have different reproducibility
// meanings, just as they do for Env.
func (b *BaselineProvenance) UnmarshalJSON(data []byte) error {
	type plainBaseline BaselineProvenance
	var decoded plainBaseline
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*b = BaselineProvenance(decoded)
	b.metadataPresent = true
	_, b.dirtyPresent = fields["dirty"]
	return nil
}

// GenerateBaselineProvenance creates the canonical policy record for an
// environment. It is suitable for serializing next to a human-readable
// baseline table.
func GenerateBaselineProvenance(env Env) BaselineProvenance {
	env = NormalizeEnv(env)
	env.metadataPresent = true
	env.dirtyPresent = true
	scenarios := make([]BaselineScenario, 0, len(ScenarioPolicyKeys()))
	for _, key := range ScenarioPolicyKeys() {
		policy, ok := ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario)
		if !ok {
			continue
		}
		scenarios = append(scenarios, BaselineScenario{
			Backend:             key.Backend,
			Library:             key.Library,
			Scenario:            key.Scenario,
			Image:               policy.Image,
			ImageDigest:         policy.ImageDigest,
			WorkloadCacheStates: append([]string(nil), policy.WorkloadCacheStates...),
			RyukCacheStates:     append([]string(nil), policy.CacheStates...),
			Iterations:          policy.Iterations,
		})
	}
	return BaselineProvenance{
		SchemaVersion: CurrentSchemaVersion,
		Commit:        env.Commit,
		Tree:          env.Tree,
		Dirty:         env.Dirty,
		Env:           env,
		Scenarios:     scenarios,

		metadataPresent: true,
		dirtyPresent:    true,
	}
}

// ValidateBaselineProvenance verifies both the environment metadata and every
// scenario input, rather than trusting a table's shape or row count.
func ValidateBaselineProvenance(baseline BaselineProvenance) error {
	if baseline.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("baseline schema_version = %d, want %d", baseline.SchemaVersion, CurrentSchemaVersion)
	}
	if baseline.metadataPresent && !baseline.dirtyPresent {
		return fmt.Errorf("baseline dirty is required")
	}
	if err := validateEnvironment(baseline.Env); err != nil {
		return fmt.Errorf("baseline env: %w", err)
	}
	if err := validateCompleteBaselineEnvironment(baseline.Env); err != nil {
		return fmt.Errorf("baseline env: %w", err)
	}
	if baseline.Commit != baseline.Env.Commit || baseline.Tree != baseline.Env.Tree || baseline.Dirty != baseline.Env.Dirty {
		return fmt.Errorf("baseline source fields do not match env provenance")
	}
	if baseline.Dirty {
		return fmt.Errorf("baseline source must be clean")
	}
	expectedKeys := ScenarioPolicyKeys()
	if len(baseline.Scenarios) != len(expectedKeys) {
		return fmt.Errorf("baseline has %d scenarios, want %d", len(baseline.Scenarios), len(expectedKeys))
	}
	seen := make(map[ScenarioKey]bool, len(baseline.Scenarios))
	for i, scenario := range baseline.Scenarios {
		key := ScenarioKey{Backend: scenario.Backend, Library: scenario.Library, Scenario: scenario.Scenario}
		expectedKey := expectedKeys[i]
		if key != expectedKey {
			return fmt.Errorf("baseline scenario %d is %s/%s/%s, want %s/%s/%s in deterministic policy order", i, key.Backend, key.Library, key.Scenario, expectedKey.Backend, expectedKey.Library, expectedKey.Scenario)
		}
		if seen[key] {
			return fmt.Errorf("baseline has duplicate scenario %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
		seen[key] = true
		policy, ok := ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario)
		if !ok {
			return fmt.Errorf("baseline has unknown scenario %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
		if scenario.Image != policy.Image || scenario.ImageDigest != policy.ImageDigest {
			return fmt.Errorf("baseline %s/%s/%s image provenance does not match policy", key.Backend, key.Library, key.Scenario)
		}
		if scenario.Iterations != policy.Iterations {
			return fmt.Errorf("baseline %s/%s/%s iterations = %d, want %d", key.Backend, key.Library, key.Scenario, scenario.Iterations, policy.Iterations)
		}
		if !sameStrings(scenario.WorkloadCacheStates, policy.WorkloadCacheStates) {
			return fmt.Errorf("baseline %s/%s/%s workload cache states do not match policy", key.Backend, key.Library, key.Scenario)
		}
		if !sameStrings(scenario.RyukCacheStates, policy.CacheStates) {
			return fmt.Errorf("baseline %s/%s/%s Ryuk cache states do not match policy", key.Backend, key.Library, key.Scenario)
		}
	}
	for _, key := range expectedKeys {
		if !seen[key] {
			return fmt.Errorf("baseline is missing scenario %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
