package bench

import "strings"

// Benchmark images are immutable references. The digest is the OCI index
// digest, so the same reference selects the same image on each supported
// platform.
const (
	RedisImage       = "public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"
	RedisImageDigest = "sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"
	NginxImage       = "public.ecr.aws/docker/library/nginx@sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f"
	NginxImageDigest = "sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f"

	// testcontainers-go 0.44.0 requests testcontainers/ryuk:0.14.0.
	// The harness maps that mutable tag to this immutable content and verifies
	// the resolved digest before accepting a result.
	TestcontainersRyukTag         = "testcontainers/ryuk:0.14.0"
	TestcontainersRyukImage       = "testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0"
	TestcontainersRyukImageDigest = "sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0"

	// Pinned aliases make the intent explicit at call sites while the
	// shorter names remain convenient for schema consumers.
	PinnedRedisImage                    = RedisImage
	PinnedRedisImageDigest              = RedisImageDigest
	PinnedNginxImage                    = NginxImage
	PinnedNginxImageDigest              = NginxImageDigest
	PinnedTestcontainersRyukImage       = TestcontainersRyukImage
	PinnedTestcontainersRyukImageDigest = TestcontainersRyukImageDigest

	// DefaultIterations is the repetition count for ordinary scenarios.
	DefaultIterations = 5
	// SessionInitIterations records the one-time testcontainers session
	// initialization separately from steady-state iterations.
	SessionInitIterations = 1

	// CacheStateCold and CacheStateWarm identify whether the relevant image
	// was absent or present when a measurement began.
	CacheStateCold = "cold"
	CacheStateWarm = "warm"
)

// ScenarioPolicy is the immutable input contract for one benchmark
// scenario. The policy is shared by the root counting harness, the
// comparison harness, and the documentation schema test.
//
// WorkloadCacheStates describes the workload image state at timer start.
// CacheStates is the independent testcontainers Ryuk state and is non-empty
// only for tc/session-init, whose result must select exactly one state.
type ScenarioPolicy struct {
	Name                string
	Image               string
	ImageDigest         string
	RyukImage           string
	RyukImageDigest     string
	WorkloadCacheStates []string
	CacheStates         []string
	Iterations          int
}

// ScenarioKey identifies the backend/library combination to which a
// scenario policy applies.
type ScenarioKey struct {
	Backend  string
	Library  string
	Scenario string
}

type scenarioPolicyRow struct {
	Policy     ScenarioPolicy
	Identities []ScenarioKey
}

var scenarioPolicyRows = []scenarioPolicyRow{
	{
		Policy: ScenarioPolicy{Name: "run/cold", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateCold}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/cold"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/cold"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/warm", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/warm"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/warm"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/warm-nginx", Image: NginxImage, ImageDigest: NginxImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/warm-nginx"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/warm-nginx"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/no-wait", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/no-wait"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/no-wait"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/forlog", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/forlog"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/forlog"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/forexec", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/forexec"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/forexec"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/parallel-8", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/parallel-8"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/parallel-8"},
		},
	},
	{
		Policy: ScenarioPolicy{Name: "run/multi-5", Image: RedisImage, ImageDigest: RedisImageDigest, WorkloadCacheStates: []string{CacheStateWarm}, Iterations: DefaultIterations},
		Identities: []ScenarioKey{
			{Backend: "docker", Library: LibraryContainerGo, Scenario: "run/multi-5"},
			{Backend: "apple", Library: LibraryContainerGo, Scenario: "run/multi-5"},
		},
	},
	{
		Policy: ScenarioPolicy{
			Name:                "tc/session-init",
			Image:               RedisImage,
			ImageDigest:         RedisImageDigest,
			RyukImage:           TestcontainersRyukImage,
			RyukImageDigest:     TestcontainersRyukImageDigest,
			WorkloadCacheStates: []string{CacheStateWarm},
			CacheStates:         []string{CacheStateCold, CacheStateWarm},
			Iterations:          SessionInitIterations,
		},
		Identities: []ScenarioKey{{Backend: "docker", Library: LibraryTestcontainersGo, Scenario: "tc/session-init"}},
	},
	{
		Policy: ScenarioPolicy{
			Name:                "tc/single",
			Image:               RedisImage,
			ImageDigest:         RedisImageDigest,
			RyukImage:           TestcontainersRyukImage,
			RyukImageDigest:     TestcontainersRyukImageDigest,
			WorkloadCacheStates: []string{CacheStateWarm},
			Iterations:          DefaultIterations,
		},
		Identities: []ScenarioKey{{Backend: "docker", Library: LibraryTestcontainersGo, Scenario: "tc/single"}},
	},
	{
		Policy: ScenarioPolicy{
			Name:                "tc/multi-5",
			Image:               RedisImage,
			ImageDigest:         RedisImageDigest,
			RyukImage:           TestcontainersRyukImage,
			RyukImageDigest:     TestcontainersRyukImageDigest,
			WorkloadCacheStates: []string{CacheStateWarm},
			Iterations:          DefaultIterations,
		},
		Identities: []ScenarioKey{{Backend: "docker", Library: LibraryTestcontainersGo, Scenario: "tc/multi-5"}},
	},
}

// ScenarioPolicies returns the documented scenario definitions in
// documentation order. It is the compatibility API for consumers that key
// policies only by scenario name; use ScenarioPoliciesFor or
// ScenarioPolicyForKey when backend/library identity is available.
func ScenarioPolicies() []ScenarioPolicy {
	policies := make([]ScenarioPolicy, 0, len(scenarioPolicyRows))
	for _, row := range scenarioPolicyRows {
		policies = append(policies, cloneScenarioPolicy(row.Policy))
	}
	return policies
}

// ScenarioPoliciesFor returns every policy applicable to one
// backend/library identity, in documentation order.
func ScenarioPoliciesFor(backend, library string) []ScenarioPolicy {
	var policies []ScenarioPolicy
	for _, row := range scenarioPolicyRows {
		for _, identity := range row.Identities {
			if identity.Backend == backend && identity.Library == library {
				policies = append(policies, cloneScenarioPolicy(row.Policy))
				break
			}
		}
	}
	return policies
}

// ScenarioPolicyFor returns the name-only policy retained for compatibility.
// Result validation uses ScenarioPolicyForKey so a name cannot be detached
// from its backend/library identity.
func ScenarioPolicyFor(name string) (ScenarioPolicy, bool) {
	for _, row := range scenarioPolicyRows {
		if row.Policy.Name == name {
			return cloneScenarioPolicy(row.Policy), true
		}
	}
	return ScenarioPolicy{}, false
}

// ScenarioPolicyForKey returns the policy for one backend/library/scenario
// key.
func ScenarioPolicyForKey(backend, library, name string) (ScenarioPolicy, bool) {
	for _, row := range scenarioPolicyRows {
		if row.Policy.Name != name {
			continue
		}
		for _, identity := range row.Identities {
			if identity.Backend == backend && identity.Library == library {
				return cloneScenarioPolicy(row.Policy), true
			}
		}
	}
	return ScenarioPolicy{}, false
}

// ScenarioPolicyKeys returns every backend/library/scenario identity in
// documentation order. It is the complete key set that strict docs and
// scenario-set validation must cover.
func ScenarioPolicyKeys() []ScenarioKey {
	keys := make([]ScenarioKey, 0, len(scenarioPolicyRows))
	for _, row := range scenarioPolicyRows {
		keys = append(keys, row.Identities...)
	}
	return keys
}

// ScenarioNames returns the documented scenario names in policy order.
func ScenarioNames() []string {
	policies := ScenarioPolicies()
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	return names
}

func cloneScenarioPolicy(policy ScenarioPolicy) ScenarioPolicy {
	policy.WorkloadCacheStates = append([]string(nil), policy.WorkloadCacheStates...)
	policy.CacheStates = append([]string(nil), policy.CacheStates...)
	return policy
}

// ImageDigest returns the digest embedded in an immutable image reference.
// It returns an empty string for a mutable or malformed reference.
func ImageDigest(image string) string {
	_, digest, ok := strings.Cut(image, "@")
	if !ok || !validSHA256Digest(digest) {
		return ""
	}
	return digest
}

func validSHA256Digest(digest string) bool {
	const prefix = "sha256:"
	if len(digest) != len(prefix)+64 || !strings.HasPrefix(digest, prefix) {
		return false
	}
	for _, c := range digest[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
