package bench

import (
	"encoding/json"
	"strings"
	"testing"
)

func completeBaselineTestEnv(commit string) Env {
	env := testEnv(commit)
	env.CLIs[AppleClientVersionKey] = "1.3.0"
	env.CLIs[AppleServiceVersionKey] = "container-apiserver version 1.3.0 (build: release, commit: abc)"
	return env
}

func TestBaselineProvenanceRoundTripAndValidation(t *testing.T) {
	baseline := GenerateBaselineProvenance(completeBaselineTestEnv(testCommit))
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	var parsed BaselineProvenance
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBaselineProvenance(parsed); err != nil {
		t.Fatalf("generated baseline is invalid: %v", err)
	}
	if len(parsed.Scenarios) != len(ScenarioPolicyKeys()) {
		t.Fatalf("scenarios = %d, want %d", len(parsed.Scenarios), len(ScenarioPolicyKeys()))
	}
	parsed.Scenarios[0].ImageDigest = RedisImageDigest
	if err := ValidateBaselineProvenance(parsed); err != nil {
		t.Fatalf("same baseline provenance was rejected: %v", err)
	}
	parsed.Scenarios[0].ImageDigest = NginxImageDigest
	if err := ValidateBaselineProvenance(parsed); err == nil || !strings.Contains(err.Error(), "image provenance") {
		t.Fatalf("mutated baseline error = %v, want image provenance error", err)
	}
}

func TestValidateBaselineProvenanceRequiresCompleteEnvironment(t *testing.T) {
	baseline := GenerateBaselineProvenance(completeBaselineTestEnv(testCommit))
	delete(baseline.Env.CLIs, AppleServiceVersionKey)
	if err := ValidateBaselineProvenance(baseline); err == nil || !strings.Contains(err.Error(), "apple.service") {
		t.Fatalf("missing Apple service error = %v", err)
	}

	baseline = GenerateBaselineProvenance(completeBaselineTestEnv(testCommit))
	baseline.Env.ReaperSessionID = ""
	if err := ValidateBaselineProvenance(baseline); err == nil || !strings.Contains(err.Error(), "reaper_session_id") {
		t.Fatalf("missing reaper session error = %v", err)
	}
}

func TestValidateBaselineProvenanceRequiresDeterministicScenarioOrder(t *testing.T) {
	baseline := GenerateBaselineProvenance(completeBaselineTestEnv(testCommit))
	baseline.Scenarios[0], baseline.Scenarios[1] = baseline.Scenarios[1], baseline.Scenarios[0]
	if err := ValidateBaselineProvenance(baseline); err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("shuffled baseline error = %v, want deterministic order error", err)
	}
}

func TestBaselineProvenanceRequiresExplicitDirtyField(t *testing.T) {
	baseline := GenerateBaselineProvenance(completeBaselineTestEnv(testCommit))
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "dirty")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var parsed BaselineProvenance
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBaselineProvenance(parsed); err == nil || !strings.Contains(err.Error(), "baseline dirty") {
		t.Fatalf("missing baseline dirty error = %v", err)
	}
}
