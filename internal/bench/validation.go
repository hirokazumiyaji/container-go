package bench

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateDoc checks the metadata contract for a newly recorded result
// document. ParseDoc intentionally remains permissive so documents written
// by older harnesses can still be read; callers that need reproducibility
// can opt into this strict check.
func ValidateDoc(d Doc) error {
	if d.Env.OS == "" {
		return fmt.Errorf("env.os is required")
	}
	if d.Env.Arch == "" {
		return fmt.Errorf("env.arch is required")
	}
	if d.Env.Host == "" {
		return fmt.Errorf("env.host is required")
	}
	if d.Env.Go == "" {
		return fmt.Errorf("env.go is required")
	}
	if d.Env.CPUs < 1 {
		return fmt.Errorf("env.cpus = %d, want >= 1", d.Env.CPUs)
	}
	if len(d.Env.CLIs) == 0 {
		return fmt.Errorf("env.clis is required")
	}
	if d.Env.Commit == "" {
		return fmt.Errorf("env.commit is required")
	}
	if d.Env.RecordedAt.IsZero() {
		return fmt.Errorf("env.recorded_at is required")
	}
	if len(d.Results) == 0 {
		return fmt.Errorf("doc has no results")
	}
	groups := make(map[string][]Result)
	for i, result := range d.Results {
		if err := ValidateResult(result); err != nil {
			return fmt.Errorf("result[%d]: %w", i, err)
		}
		if result.Commit != d.Env.Commit {
			return fmt.Errorf("result[%d] commit = %q, want %q", i, result.Commit, d.Env.Commit)
		}
		key := result.Backend + "\x00" + result.Library + "\x00" + result.Image + "\x00" + result.Scenario
		groups[key] = append(groups[key], result)
	}
	return validateResultGroups(groups)
}

// ValidateResult checks one result's identity, pinned image, and iteration
// metadata against the scenario policy.
func ValidateResult(result Result) error {
	if result.Backend == "" {
		return fmt.Errorf("backend is required")
	}
	if result.Library == "" {
		return fmt.Errorf("library is required")
	}
	if result.Image == "" {
		return fmt.Errorf("image is required")
	}
	if result.Scenario == "" {
		return fmt.Errorf("scenario is required")
	}
	if result.Commit == "" {
		return fmt.Errorf("commit is required")
	}
	if result.Iteration < 1 {
		return fmt.Errorf("iteration = %d, want >= 1", result.Iteration)
	}
	if result.Iterations < 1 {
		return fmt.Errorf("iterations = %d, want >= 1", result.Iterations)
	}
	policy, ok := ScenarioPolicyFor(result.Scenario)
	if !ok {
		return fmt.Errorf("unknown scenario %q", result.Scenario)
	}
	if result.Iterations != policy.Iterations {
		return fmt.Errorf("%s iterations = %d, want %d", result.Scenario, result.Iterations, policy.Iterations)
	}
	if result.Iteration > result.Iterations {
		return fmt.Errorf("%s iteration = %d, want <= %d", result.Scenario, result.Iteration, result.Iterations)
	}
	if result.Image != policy.Image {
		return fmt.Errorf("%s image = %q, want pinned image %q", result.Scenario, result.Image, policy.Image)
	}
	if result.ImageDigest != policy.ImageDigest {
		return fmt.Errorf("%s image digest = %q, want %q", result.Scenario, result.ImageDigest, policy.ImageDigest)
	}
	if referenceDigest := ImageDigest(result.Image); referenceDigest == "" {
		return fmt.Errorf("%s image %q is not pinned to a sha256 digest", result.Scenario, result.Image)
	} else if referenceDigest != result.ImageDigest {
		return fmt.Errorf("%s image reference digest = %q, recorded %q", result.Scenario, referenceDigest, result.ImageDigest)
	}
	if result.DurationNS < 0 {
		return fmt.Errorf("duration is negative")
	}
	if result.Subprocesses < 0 {
		return fmt.Errorf("subprocesses is negative")
	}
	return nil
}

// ValidateScenarioSet checks a complete result collection. Each scenario
// group must contain exactly one result for every planned iteration, and
// every documented scenario must be represented at least once.
func ValidateScenarioSet(results []Result) error {
	if len(results) == 0 {
		return fmt.Errorf("scenario set has no results")
	}
	groups := make(map[string][]Result)
	seen := make(map[string]bool)
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
		key := result.Backend + "\x00" + result.Library + "\x00" + result.Image + "\x00" + result.Scenario
		groups[key] = append(groups[key], result)
		seen[result.Scenario] = true
	}
	if err := validateResultGroups(groups); err != nil {
		return err
	}
	for _, name := range ScenarioNames() {
		if !seen[name] {
			return fmt.Errorf("scenario set is missing %q", name)
		}
	}
	return nil
}

func validateResultGroups(groups map[string][]Result) error {
	for _, group := range groups {
		policy, ok := ScenarioPolicyFor(group[0].Scenario)
		if !ok {
			return fmt.Errorf("unknown scenario %q", group[0].Scenario)
		}
		if len(group) != policy.Iterations {
			return fmt.Errorf("%s has %d results, want %d", group[0].Scenario, len(group), policy.Iterations)
		}
		iterations := make(map[int]bool, len(group))
		for _, result := range group {
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

// CompareDocs checks whether two result documents have comparable inputs.
// It deliberately compares metadata as well as scenario groups, so changing
// a mutable tag or the source revision fails before numbers are compared.
func CompareDocs(baseline, candidate Doc) error {
	if len(baseline.Results) == 0 || len(candidate.Results) == 0 {
		return fmt.Errorf("both documents must contain results")
	}
	if baseline.Env.Commit == "" || candidate.Env.Commit == "" {
		return fmt.Errorf("both documents must record a commit")
	}
	if baseline.Env.Commit != candidate.Env.Commit {
		return fmt.Errorf("commit mismatch: baseline %q, candidate %q", baseline.Env.Commit, candidate.Env.Commit)
	}
	if err := compareEnvironment(baseline.Env, candidate.Env); err != nil {
		return err
	}

	baselineGroups := resultGroups(baseline.Results)
	candidateGroups := resultGroups(candidate.Results)
	for key, baselineGroup := range baselineGroups {
		candidateGroup, ok := candidateGroups[key]
		if !ok {
			return fmt.Errorf("candidate is missing result group %q", displayGroupKey(key))
		}
		if err := compareGroups(baselineGroup, candidateGroup); err != nil {
			return err
		}
	}
	for key := range candidateGroups {
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
	for name, version := range baseline.CLIs {
		if candidate.CLIs[name] != version {
			return fmt.Errorf("CLI %q version mismatch: baseline %q, candidate %q", name, version, candidate.CLIs[name])
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
		key := result.Backend + "\x00" + result.Library + "\x00" + result.Scenario
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
	sort.SliceStable(baselineResults, func(i, j int) bool {
		return baselineResults[i].Iteration < baselineResults[j].Iteration
	})
	sort.SliceStable(candidateResults, func(i, j int) bool {
		return candidateResults[i].Iteration < candidateResults[j].Iteration
	})
	seen := make(map[int]bool, len(baselineResults))
	for i := range baselineResults {
		first := baselineResults[i]
		other := candidateResults[i]
		if seen[first.Iteration] {
			return fmt.Errorf("duplicate iteration %d for %q", first.Iteration, first.Scenario)
		}
		seen[first.Iteration] = true
		if first.Iteration != other.Iteration {
			return fmt.Errorf("iteration values mismatch for %q", first.Scenario)
		}
		if first.Image != other.Image {
			return fmt.Errorf("image reference mismatch for %q: baseline %q, candidate %q", first.Scenario, first.Image, other.Image)
		}
		if first.ImageDigest != other.ImageDigest {
			return fmt.Errorf("image digest mismatch for %q: baseline %q, candidate %q", first.Scenario, first.ImageDigest, other.ImageDigest)
		}
		if first.Iterations != other.Iterations {
			return fmt.Errorf("iteration policy mismatch for %q: baseline %d, candidate %d", first.Scenario, first.Iterations, other.Iterations)
		}
		if first.Commit != other.Commit {
			return fmt.Errorf("result commit mismatch for %q: baseline %q, candidate %q", first.Scenario, first.Commit, other.Commit)
		}
	}
	return nil
}

func displayGroupKey(key string) string {
	return strings.ReplaceAll(key, "\x00", "/")
}
