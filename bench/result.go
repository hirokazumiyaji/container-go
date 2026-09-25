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
)

// Library names a code path under measurement.
const (
	LibraryContainerGo      = ibench.LibraryContainerGo
	LibraryTestcontainersGo = ibench.LibraryTestcontainersGo

	RedisImage             = ibench.RedisImage
	RedisImageDigest       = ibench.RedisImageDigest
	NginxImage             = ibench.NginxImage
	NginxImageDigest       = ibench.NginxImageDigest
	PinnedRedisImage       = ibench.PinnedRedisImage
	PinnedRedisImageDigest = ibench.PinnedRedisImageDigest
	PinnedNginxImage       = ibench.PinnedNginxImage
	PinnedNginxImageDigest = ibench.PinnedNginxImageDigest
	DefaultIterations      = ibench.DefaultIterations
	SessionInitIterations  = ibench.SessionInitIterations
)

// Summarize groups results by (backend, library, image, scenario) and
// aggregates each group's durations.
func Summarize(results []Result) []Summary { return ibench.Summarize(results) }

// ScenarioPolicies returns the documented benchmark scenario contract.
func ScenarioPolicies() []ScenarioPolicy { return ibench.ScenarioPolicies() }

// ScenarioNames returns the documented benchmark scenario names.
func ScenarioNames() []string { return ibench.ScenarioNames() }

// ScenarioPolicyFor returns the policy for a scenario name.
func ScenarioPolicyFor(name string) (ScenarioPolicy, bool) { return ibench.ScenarioPolicyFor(name) }

// ImageDigest returns the digest embedded in an immutable image reference.
func ImageDigest(image string) string { return ibench.ImageDigest(image) }

// CurrentCommit resolves the source revision used for a benchmark run.
func CurrentCommit() (string, error) { return ibench.CurrentCommit() }

// ValidateDoc checks that a result document satisfies the reproducible
// benchmark metadata contract.
func ValidateDoc(d Doc) error { return ibench.ValidateDoc(d) }

// ValidateScenarioSet checks that a complete result collection contains
// every documented scenario with its expected iteration count.
func ValidateScenarioSet(results []Result) error { return ibench.ValidateScenarioSet(results) }

// CompareDocs rejects baseline comparisons whose source revision, image
// digest, scenario inputs, or iteration policy differ.
func CompareDocs(baseline, candidate Doc) error { return ibench.CompareDocs(baseline, candidate) }

// Table renders summaries as a fixed-width human-readable table.
func Table(summaries []Summary) string { return ibench.Table(summaries) }

// WriteJSON writes the doc as formatted JSON.
func WriteJSON(w io.Writer, d Doc) error { return d.WriteJSON(w) }

// ParseDoc reads a doc written by WriteJSON.
func ParseDoc(data []byte) (Doc, error) { return ibench.ParseDoc(data) }
