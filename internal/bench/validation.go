package bench

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateDoc checks the strict metadata and complete scenario contract for
// a newly recorded result document. ParseDoc intentionally remains
// permissive so historical documents can still be inspected.
func ValidateDoc(d Doc) error {
	if d.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", d.SchemaVersion, CurrentSchemaVersion)
	}
	if err := validateEnvironment(d.Env); err != nil {
		return err
	}
	if len(d.Results) == 0 {
		return fmt.Errorf("doc has no results")
	}
	for i, result := range d.Results {
		if err := ValidateResult(result); err != nil {
			return fmt.Errorf("result[%d]: %w", i, err)
		}
		if result.Commit != d.Env.Commit {
			return fmt.Errorf("result[%d] commit = %q, want %q", i, result.Commit, d.Env.Commit)
		}
	}
	if err := validateVersionMetadata(d.Env, d.Results); err != nil {
		return err
	}
	if err := ValidateScenarioSet(d.Results); err != nil {
		return fmt.Errorf("scenario set: %w", err)
	}
	return nil
}

func validateEnvironment(env Env) error {
	if env.metadataPresent && !env.dirtyPresent {
		return fmt.Errorf("env.dirty is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "os", value: env.OS},
		{name: "arch", value: env.Arch},
		{name: "go", value: env.Go},
		{name: "host", value: env.Host},
	} {
		if err := validateMetadataValue("env."+field.name, field.value); err != nil {
			return err
		}
	}
	if strings.EqualFold(env.Host, "unknown") {
		return fmt.Errorf("env.host must not be unknown")
	}
	if env.CPUs < 1 {
		return fmt.Errorf("env.cpus = %d, want >= 1", env.CPUs)
	}
	if !validGitObjectID(env.Commit) {
		return fmt.Errorf("env.commit = %q, want a full Git object ID", env.Commit)
	}
	if !validGitObjectID(env.Tree) {
		return fmt.Errorf("env.tree = %q, want a full Git tree object ID", env.Tree)
	}
	if env.Dirty {
		return fmt.Errorf("env.dirty is true; benchmark source must be clean")
	}
	if len(env.CLIs) == 0 {
		return fmt.Errorf("env.clis is required")
	}
	allowedCLIs := map[string]bool{
		DockerClientVersionKey: true,
		DockerServerVersionKey: true,
		AppleClientVersionKey:  true,
		AppleServiceVersionKey: true,
	}
	for name, version := range env.CLIs {
		if !allowedCLIs[name] {
			return fmt.Errorf("env.clis[%q] is not a recognized backend version key", name)
		}
		if err := validateMetadataValue("env.clis["+name+"]", version); err != nil {
			return err
		}
		if strings.EqualFold(version, "unknown") {
			return fmt.Errorf("env.clis[%q] must not be unknown", name)
		}
	}
	if env.ReaperSessionID != "" {
		if err := validateMetadataValue("env.reaper_session_id", env.ReaperSessionID); err != nil {
			return err
		}
		if !validSessionID(env.ReaperSessionID) {
			return fmt.Errorf("env.reaper_session_id = %q, want a normalized session ID", env.ReaperSessionID)
		}
	}
	if env.RecordedAt.IsZero() {
		return fmt.Errorf("env.recorded_at is required")
	}
	return nil
}

func validateMetadataValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must be normalized", name)
	}
	return nil
}

func validateVersionMetadata(env Env, results []Result) error {
	required := make(map[string]bool)
	usesTestcontainers := false
	usesDocker := false
	usesApple := false
	for _, result := range results {
		switch result.Backend {
		case "docker":
			usesDocker = true
			required[DockerClientVersionKey] = true
			required[DockerServerVersionKey] = true
		case "apple":
			usesApple = true
			required[AppleClientVersionKey] = true
		}
		if result.Library == LibraryTestcontainersGo {
			usesTestcontainers = true
		}
	}
	for name := range env.CLIs {
		if usesDocker && strings.HasPrefix(name, "apple.") {
			return fmt.Errorf("env.clis[%q] is not valid for a Docker-only result set", name)
		}
		if usesApple && strings.HasPrefix(name, "docker.") {
			return fmt.Errorf("env.clis[%q] is not valid for an Apple-only result set", name)
		}
	}
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(env.CLIs[key]) == "" {
			return fmt.Errorf("env.clis[%q] is required for the recorded backends", key)
		}
	}
	if usesTestcontainers {
		if env.ReaperSessionID == "" {
			return fmt.Errorf("env.reaper_session_id is required for testcontainers results")
		}
	} else if env.ReaperSessionID != "" {
		return fmt.Errorf("env.reaper_session_id is only valid for testcontainers results")
	}
	return nil
}

// ValidateResult checks one result's backend/library identity, immutable
// images, optional Ryuk provenance, and iteration metadata against policy.
func ValidateResult(result Result) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "backend", value: result.Backend},
		{name: "library", value: result.Library},
		{name: "image", value: result.Image},
		{name: "scenario", value: result.Scenario},
		{name: "commit", value: result.Commit},
	} {
		if err := validateMetadataValue(field.name, field.value); err != nil {
			return err
		}
	}
	if result.WorkloadCacheState == "" {
		return fmt.Errorf("workload_cache_state is required")
	}
	if strings.TrimSpace(result.WorkloadCacheState) != result.WorkloadCacheState {
		return fmt.Errorf("workload_cache_state must be normalized")
	}
	if !validGitObjectID(result.Commit) {
		return fmt.Errorf("commit = %q, want a full Git object ID", result.Commit)
	}
	if result.Iteration < 1 {
		return fmt.Errorf("iteration = %d, want >= 1", result.Iteration)
	}
	if result.Iterations < 1 {
		return fmt.Errorf("iterations = %d, want >= 1", result.Iterations)
	}
	policy, ok := ScenarioPolicyForKey(result.Backend, result.Library, result.Scenario)
	if !ok {
		return fmt.Errorf("unknown scenario key %s/%s/%s", result.Backend, result.Library, result.Scenario)
	}
	if result.Iterations != policy.Iterations {
		return fmt.Errorf("%s iterations = %d, want %d", result.Scenario, result.Iterations, policy.Iterations)
	}
	if result.Iteration > result.Iterations {
		return fmt.Errorf("%s iteration = %d, want <= %d", result.Scenario, result.Iteration, result.Iterations)
	}
	if len(policy.WorkloadCacheStates) == 0 || !containsString(policy.WorkloadCacheStates, result.WorkloadCacheState) {
		return fmt.Errorf("%s workload_cache_state = %q, want one of %v", result.Scenario, result.WorkloadCacheState, policy.WorkloadCacheStates)
	}
	if result.Image != policy.Image {
		return fmt.Errorf("%s image = %q, want pinned image %q", result.Scenario, result.Image, policy.Image)
	}
	if result.ImageDigest != policy.ImageDigest {
		return fmt.Errorf("%s image digest = %q, want %q", result.Scenario, result.ImageDigest, policy.ImageDigest)
	}
	if referenceDigest := ImageDigest(result.Image); referenceDigest == "" {
		return fmt.Errorf("%s image %q is not pinned to a valid sha256 digest", result.Scenario, result.Image)
	} else if referenceDigest != result.ImageDigest {
		return fmt.Errorf("%s image reference digest = %q, recorded %q", result.Scenario, referenceDigest, result.ImageDigest)
	}

	if policy.RyukImage == "" {
		if result.RyukImage != "" || result.RyukImageDigest != "" {
			return fmt.Errorf("%s has unexpected Ryuk image metadata", result.Scenario)
		}
	} else {
		if result.RyukImage != policy.RyukImage {
			return fmt.Errorf("%s Ryuk image = %q, want pinned image %q", result.Scenario, result.RyukImage, policy.RyukImage)
		}
		if result.RyukImageDigest != policy.RyukImageDigest {
			return fmt.Errorf("%s Ryuk image digest = %q, want %q", result.Scenario, result.RyukImageDigest, policy.RyukImageDigest)
		}
		if referenceDigest := ImageDigest(result.RyukImage); referenceDigest == "" {
			return fmt.Errorf("%s Ryuk image %q is not pinned to a valid sha256 digest", result.Scenario, result.RyukImage)
		} else if referenceDigest != result.RyukImageDigest {
			return fmt.Errorf("%s Ryuk image reference digest = %q, recorded %q", result.Scenario, referenceDigest, result.RyukImageDigest)
		}
	}

	if len(policy.CacheStates) == 0 {
		if result.CacheState != "" {
			return fmt.Errorf("%s cache_state = %q, want empty", result.Scenario, result.CacheState)
		}
	} else if !containsString(policy.CacheStates, result.CacheState) {
		return fmt.Errorf("%s cache_state = %q, want one of %v", result.Scenario, result.CacheState, policy.CacheStates)
	}
	if result.DurationNS < 0 {
		return fmt.Errorf("duration is negative")
	}
	if result.Subprocesses < 0 {
		return fmt.Errorf("subprocesses is negative")
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ValidateScenarioSet checks that every result is valid, every backend and
// library identity has the exact policy-defined scenario set, and every
// group has each planned iteration exactly once.
func ValidateScenarioSet(results []Result) error {
	if len(results) == 0 {
		return fmt.Errorf("scenario set has no results")
	}
	groups := make(map[string][]Result)
	identities := make(map[scenarioIdentity]map[string]bool)
	commit := ""
	for i, result := range results {
		if err := ValidateResult(result); err != nil {
			return fmt.Errorf("result[%d]: %w", i, err)
		}
		if commit == "" {
			commit = result.Commit
		} else if result.Commit != commit {
			return fmt.Errorf("result[%d] commit = %q, want %q", i, result.Commit, commit)
		}
		key := resultGroupKey(result)
		groups[key] = append(groups[key], result)
		identity := scenarioIdentity{Backend: result.Backend, Library: result.Library}
		if identities[identity] == nil {
			identities[identity] = make(map[string]bool)
		}
		identities[identity][result.Scenario] = true
	}
	if err := validateResultGroups(groups); err != nil {
		return err
	}

	identityKeys := make([]scenarioIdentity, 0, len(identities))
	for identity := range identities {
		identityKeys = append(identityKeys, identity)
	}
	sort.Slice(identityKeys, func(i, j int) bool {
		if identityKeys[i].Backend != identityKeys[j].Backend {
			return identityKeys[i].Backend < identityKeys[j].Backend
		}
		return identityKeys[i].Library < identityKeys[j].Library
	})
	for _, identity := range identityKeys {
		policies := ScenarioPoliciesFor(identity.Backend, identity.Library)
		if len(policies) == 0 {
			return fmt.Errorf("no policies for backend/library %s/%s", identity.Backend, identity.Library)
		}
		for _, policy := range policies {
			if !identities[identity][policy.Name] {
				return fmt.Errorf("%s/%s is missing %q", identity.Backend, identity.Library, policy.Name)
			}
		}
	}
	return nil
}

func validateResultGroups(groups map[string][]Result) error {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		policy, ok := ScenarioPolicyForKey(group[0].Backend, group[0].Library, group[0].Scenario)
		if !ok {
			return fmt.Errorf("unknown scenario %q", group[0].Scenario)
		}
		if len(group) != policy.Iterations {
			return fmt.Errorf("%s has %d results, want %d", group[0].Scenario, len(group), policy.Iterations)
		}
		iterations := make(map[int]bool, len(group))
		workloadState := group[0].WorkloadCacheState
		ryukState := group[0].CacheState
		for _, result := range group {
			if result.WorkloadCacheState != workloadState {
				return fmt.Errorf("%s mixes workload cache states %q and %q", group[0].Scenario, workloadState, result.WorkloadCacheState)
			}
			if result.CacheState != ryukState {
				return fmt.Errorf("%s mixes Ryuk cache states %q and %q", group[0].Scenario, ryukState, result.CacheState)
			}
			if iterations[result.Iteration] {
				return fmt.Errorf("%s has duplicate iteration %d", group[0].Scenario, result.Iteration)
			}
			iterations[result.Iteration] = true
		}
		for iteration := 1; iteration <= policy.Iterations; iteration++ {
			if !iterations[iteration] {
				return fmt.Errorf("%s is missing iteration %d", group[0].Scenario, iteration)
			}
		}
	}
	return nil
}

type scenarioIdentity struct {
	Backend string
	Library string
}

func resultGroupKey(result Result) string {
	return result.Backend + "\x00" + result.Library + "\x00" + result.Scenario
}

// CompareDocs checks whether two strictly valid result documents have
// comparable inputs. Source commits and trees are intentionally excluded so
// before/after runs can compare different clean revisions.
func CompareDocs(baseline, candidate Doc) error {
	if err := ValidateDoc(baseline); err != nil {
		return fmt.Errorf("invalid baseline document: %w", err)
	}
	if err := ValidateDoc(candidate); err != nil {
		return fmt.Errorf("invalid candidate document: %w", err)
	}
	if err := compareEnvironment(baseline.Env, candidate.Env); err != nil {
		return err
	}

	baselineGroups := resultGroups(baseline.Results)
	candidateGroups := resultGroups(candidate.Results)
	keys := make([]string, 0, len(baselineGroups))
	for key := range baselineGroups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		candidateGroup, ok := candidateGroups[key]
		if !ok {
			return fmt.Errorf("candidate is missing result group %q", displayGroupKey(key))
		}
		if err := compareGroups(baselineGroups[key], candidateGroup); err != nil {
			return err
		}
	}
	keys = keys[:0]
	for key := range candidateGroups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := baselineGroups[key]; !ok {
			return fmt.Errorf("candidate has unexpected result group %q", displayGroupKey(key))
		}
	}
	return nil
}

func compareEnvironment(baseline, candidate Env) error {
	if baseline.OS != candidate.OS {
		return fmt.Errorf("OS mismatch: baseline %q, candidate %q", baseline.OS, candidate.OS)
	}
	if baseline.Arch != candidate.Arch {
		return fmt.Errorf("architecture mismatch: baseline %q, candidate %q", baseline.Arch, candidate.Arch)
	}
	if baseline.CPUs != candidate.CPUs {
		return fmt.Errorf("CPU count mismatch: baseline %d, candidate %d", baseline.CPUs, candidate.CPUs)
	}
	if baseline.Go != candidate.Go {
		return fmt.Errorf("go version mismatch: baseline %q, candidate %q", baseline.Go, candidate.Go)
	}
	if baseline.Host != candidate.Host {
		return fmt.Errorf("host mismatch: baseline %q, candidate %q", baseline.Host, candidate.Host)
	}
	if len(baseline.CLIs) != len(candidate.CLIs) {
		return fmt.Errorf("CLI version set mismatch")
	}
	keys := make([]string, 0, len(baseline.CLIs))
	for key := range baseline.CLIs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if candidate.CLIs[key] != baseline.CLIs[key] {
			return fmt.Errorf("CLI %q version mismatch: baseline %q, candidate %q", key, baseline.CLIs[key], candidate.CLIs[key])
		}
	}
	return nil
}

type resultGroup struct {
	results []Result
}

func resultGroups(results []Result) map[string]resultGroup {
	groups := make(map[string]resultGroup)
	for _, result := range results {
		key := resultGroupKey(result)
		groups[key] = resultGroup{results: append(groups[key].results, result)}
	}
	return groups
}

func compareGroups(baseline, candidate resultGroup) error {
	if len(baseline.results) == 0 || len(candidate.results) == 0 {
		return fmt.Errorf("result group is empty")
	}
	if len(baseline.results) != len(candidate.results) {
		return fmt.Errorf("result group %q has %d results, baseline has %d", baseline.results[0].Scenario, len(candidate.results), len(baseline.results))
	}

	baselineResults := append([]Result(nil), baseline.results...)
	candidateResults := append([]Result(nil), candidate.results...)
	sort.SliceStable(baselineResults, func(i, j int) bool { return baselineResults[i].Iteration < baselineResults[j].Iteration })
	sort.SliceStable(candidateResults, func(i, j int) bool { return candidateResults[i].Iteration < candidateResults[j].Iteration })
	for i := range baselineResults {
		first := baselineResults[i]
		other := candidateResults[i]
		if first.Iteration != other.Iteration {
			return fmt.Errorf("iteration values mismatch for %q", first.Scenario)
		}
		if first.Image != other.Image {
			return fmt.Errorf("image reference mismatch for %q: baseline %q, candidate %q", first.Scenario, first.Image, other.Image)
		}
		if first.ImageDigest != other.ImageDigest {
			return fmt.Errorf("image digest mismatch for %q: baseline %q, candidate %q", first.Scenario, first.ImageDigest, other.ImageDigest)
		}
		if first.WorkloadCacheState != other.WorkloadCacheState {
			return fmt.Errorf("workload cache state mismatch for %q: baseline %q, candidate %q", first.Scenario, first.WorkloadCacheState, other.WorkloadCacheState)
		}
		if first.RyukImage != other.RyukImage {
			return fmt.Errorf("ryuk image mismatch for %q: baseline %q, candidate %q", first.Scenario, first.RyukImage, other.RyukImage)
		}
		if first.RyukImageDigest != other.RyukImageDigest {
			return fmt.Errorf("ryuk image digest mismatch for %q: baseline %q, candidate %q", first.Scenario, first.RyukImageDigest, other.RyukImageDigest)
		}
		if first.CacheState != other.CacheState {
			return fmt.Errorf("cache state mismatch for %q: baseline %q, candidate %q", first.Scenario, first.CacheState, other.CacheState)
		}
		if first.Iterations != other.Iterations {
			return fmt.Errorf("iteration policy mismatch for %q: baseline %d, candidate %d", first.Scenario, first.Iterations, other.Iterations)
		}
	}
	return nil
}

func displayGroupKey(key string) string {
	return strings.ReplaceAll(key, "\x00", "/")
}
