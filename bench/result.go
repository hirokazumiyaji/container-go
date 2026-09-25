// Package bench holds the testcontainers-go comparison scenarios and
// the shared result schema surface. It is a separate module so the
// testcontainers-go dependency never enters the library's go.mod.
package bench

import (
	"io"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

// Aliases of the shared measurement schema (internal/bench). The root
// module's counting scenarios record results in the same format.
type (
	// Result is one measured iteration of one scenario.
	Result = ibench.Result
	// Env records the environment a run happened in.
	Env = ibench.Env
	// Doc is a complete recorded run: environment plus results.
	Doc = ibench.Doc
	// Summary aggregates the iterations of one scenario.
	Summary = ibench.Summary
	// ScenarioPolicy describes the pinned inputs and repetition count for
	// one documented benchmark scenario.
	ScenarioPolicy = ibench.ScenarioPolicy
	// ScenarioKey identifies a policy by backend, library, and scenario.
	ScenarioKey = ibench.ScenarioKey
	// SourceMetadata identifies the clean source revision and Git tree.
	SourceMetadata = ibench.SourceMetadata
	// Source is a shorter alias for SourceMetadata.
	Source = ibench.Source
)

// Library names a code path under measurement.
const (
	LibraryContainerGo      = ibench.LibraryContainerGo
	LibraryTestcontainersGo = ibench.LibraryTestcontainersGo

	RedisImage                          = ibench.RedisImage
	RedisImageDigest                    = ibench.RedisImageDigest
	NginxImage                          = ibench.NginxImage
	NginxImageDigest                    = ibench.NginxImageDigest
	PinnedRedisImage                    = ibench.PinnedRedisImage
	PinnedRedisImageDigest              = ibench.PinnedRedisImageDigest
	PinnedNginxImage                    = ibench.PinnedNginxImage
	PinnedNginxImageDigest              = ibench.PinnedNginxImageDigest
	TestcontainersRyukTag               = ibench.TestcontainersRyukTag
	TestcontainersRyukImage             = ibench.TestcontainersRyukImage
	TestcontainersRyukImageDigest       = ibench.TestcontainersRyukImageDigest
	PinnedTestcontainersRyukImage       = ibench.PinnedTestcontainersRyukImage
	PinnedTestcontainersRyukImageDigest = ibench.PinnedTestcontainersRyukImageDigest
	PinnedRyukImage                     = ibench.PinnedRyukImage
	PinnedRyukImageDigest               = ibench.PinnedRyukImageDigest
	RyukImage                           = ibench.TestcontainersRyukImage
	RyukImageDigest                     = ibench.TestcontainersRyukImageDigest
	CacheStateCold                      = ibench.CacheStateCold
	CacheStateWarm                      = ibench.CacheStateWarm
	DefaultIterations                   = ibench.DefaultIterations
	SessionInitIterations               = ibench.SessionInitIterations
	CurrentSchemaVersion                = ibench.CurrentSchemaVersion
	DockerClientVersionKey              = ibench.DockerClientVersionKey
	DockerServerVersionKey              = ibench.DockerServerVersionKey
	AppleClientVersionKey               = ibench.AppleClientVersionKey
	AppleServiceVersionKey              = ibench.AppleServiceVersionKey
)

// Summarize groups results by (backend, library, image, scenario) and
// aggregates each group's durations.
func Summarize(results []Result) []Summary { return ibench.Summarize(results) }

// ScenarioPolicies returns the documented benchmark scenario contract.
func ScenarioPolicies() []ScenarioPolicy { return ibench.ScenarioPolicies() }

// ScenarioNames returns the documented benchmark scenario names.
func ScenarioNames() []string { return ibench.ScenarioNames() }

// ScenarioPolicyKeys returns every backend/library/scenario identity in
// the complete benchmark policy.
func ScenarioPolicyKeys() []ScenarioKey { return ibench.ScenarioPolicyKeys() }

// ScenarioPolicyFor returns the compatibility name-only policy.
func ScenarioPolicyFor(name string) (ScenarioPolicy, bool) { return ibench.ScenarioPolicyFor(name) }

// ScenarioPoliciesFor returns all policies for a backend/library identity.
func ScenarioPoliciesFor(backend, library string) []ScenarioPolicy {
	return ibench.ScenarioPoliciesFor(backend, library)
}

// ScenarioPolicyForKey returns the policy for a backend/library/scenario key.
func ScenarioPolicyForKey(backend, library, scenario string) (ScenarioPolicy, bool) {
	return ibench.ScenarioPolicyForKey(backend, library, scenario)
}

// ImageDigest returns the digest embedded in an immutable image reference.
func ImageDigest(image string) string { return ibench.ImageDigest(image) }

// CurrentCommit resolves a verified source revision and rejects dirty Git
// source. Use RequireCleanSource for strict benchmark recording.
func CurrentCommit() (string, error) { return ibench.CurrentCommit() }

// CurrentSource returns source provenance, including dirty state and tree.
func CurrentSource() (SourceMetadata, error) { return ibench.CurrentSource() }

// RequireCleanSource rejects dirty or unverifiable benchmark source.
func RequireCleanSource() (SourceMetadata, error) { return ibench.RequireCleanSource() }

// ValidateDoc checks that a result document satisfies the reproducible
// benchmark metadata contract.
func ValidateDoc(d Doc) error { return ibench.ValidateDoc(d) }

// ValidateScenarioSet checks that a complete result collection contains
// every documented scenario with its expected iteration count.
func ValidateScenarioSet(results []Result) error { return ibench.ValidateScenarioSet(results) }

// CompareDocs rejects invalid documents and mismatched image, scenario,
// or environment inputs while allowing clean source commits to differ.
func CompareDocs(baseline, candidate Doc) error { return ibench.CompareDocs(baseline, candidate) }

// Table renders summaries as a fixed-width human-readable table.
func Table(summaries []Summary) string { return ibench.Table(summaries) }

// WriteJSON writes the doc as formatted JSON.
func WriteJSON(w io.Writer, d Doc) error { return d.WriteJSON(w) }

// ParseDoc reads a doc written by WriteJSON.
func ParseDoc(data []byte) (Doc, error) { return ibench.ParseDoc(data) }

// NormalizeEnv trims and canonicalizes environment metadata.
func NormalizeEnv(env Env) Env { return ibench.NormalizeEnv(env) }

// NormalizeDoc returns a metadata-normalized copy of a result document.
func NormalizeDoc(doc Doc) Doc { return ibench.NormalizeDoc(doc) }
