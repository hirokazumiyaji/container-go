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

	// Pinned aliases make the intent explicit at call sites while the
	// shorter names remain convenient for schema consumers.
	PinnedRedisImage       = RedisImage
	PinnedRedisImageDigest = RedisImageDigest
	PinnedNginxImage       = NginxImage
	PinnedNginxImageDigest = NginxImageDigest

	// DefaultIterations is the repetition count for ordinary scenarios.
	DefaultIterations = 5
	// SessionInitIterations records the one-time testcontainers session
	// initialization separately from steady-state iterations.
	SessionInitIterations = 1
)

// ScenarioPolicy is the immutable input contract for one benchmark
// scenario. The policy is shared by the root counting harness, the
// comparison harness, and the documentation schema test.
type ScenarioPolicy struct {
	Name        string
	Image       string
	ImageDigest string
	Iterations  int
}

// ScenarioPolicies returns the scenarios documented by the benchmark
// suite, in documentation order. A new copy is returned so callers cannot
// mutate the package's policy.
func ScenarioPolicies() []ScenarioPolicy {
	return []ScenarioPolicy{
		{Name: "run/cold", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/warm", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/warm-nginx", Image: NginxImage, ImageDigest: NginxImageDigest, Iterations: DefaultIterations},
		{Name: "run/no-wait", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/forlog", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/forexec", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/parallel-8", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "run/multi-5", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "tc/session-init", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: SessionInitIterations},
		{Name: "tc/single", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
		{Name: "tc/multi-5", Image: RedisImage, ImageDigest: RedisImageDigest, Iterations: DefaultIterations},
	}
}

// ScenarioPolicyFor returns the policy for a scenario name.
func ScenarioPolicyFor(name string) (ScenarioPolicy, bool) {
	for _, policy := range ScenarioPolicies() {
		if policy.Name == name {
			return policy, true
		}
	}
	return ScenarioPolicy{}, false
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

// ImageDigest returns the digest embedded in an immutable image
// reference. It returns an empty string for a mutable tag.
func ImageDigest(image string) string {
	_, digest, ok := strings.Cut(image, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	return digest
}
