// Package bench defines the shared measurement schema for the
// benchmark harness. The root module's integration tests and the
// separate bench module (testcontainers-go comparison) both record
// results in this format so later optimization issues can compare
// numbers across runs.
package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// CurrentSchemaVersion is the strict, reproducible result schema. ParseDoc
// remains permissive for older documents, but ValidateDoc and CompareDocs
// accept only this version.
const CurrentSchemaVersion = 2

// Library names a code path under measurement.
const (
	LibraryContainerGo      = "container-go"
	LibraryTestcontainersGo = "testcontainers-go"
)

// Result is one measured iteration of one scenario.
type Result struct {
	// Backend is the container backend: "apple" or "docker".
	Backend string `json:"backend"`
	// Library is the library under test.
	Library string `json:"library"`
	// Image is the immutable image reference the scenario started.
	Image string `json:"image"`
	// ImageDigest is the content digest resolved for Image.
	ImageDigest string `json:"image_digest,omitempty"`
	// RyukImage is the immutable reaper image used by a
	// testcontainers-go scenario. It is empty for container-go scenarios.
	RyukImage string `json:"ryuk_image,omitempty"`
	// RyukImageDigest is the content digest of RyukImage.
	RyukImageDigest string `json:"ryuk_image_digest,omitempty"`
	// CacheState is "cold" or "warm" for tc/session-init and empty for
	// every other scenario.
	CacheState string `json:"cache_state,omitempty"`
	// Scenario identifies the measurement, e.g. "run/warm".
	Scenario string `json:"scenario"`
	// Iteration is the 1-based repetition of the scenario.
	Iteration int `json:"iteration"`
	// Iterations is the total number of iterations planned for the
	// scenario. It is carried on every result so a partial document is
	// not mistaken for a complete measurement.
	Iterations int `json:"iterations,omitempty"`
	// Commit identifies the source revision that produced the result.
	Commit string `json:"commit,omitempty"`
	// DurationNS is the wall-clock time of the iteration.
	DurationNS int64 `json:"duration_ns"`
	// Subprocesses is the number of CLI child processes spawned, or
	// zero when the scenario does not count subprocesses.
	Subprocesses int64 `json:"subprocesses"`
}

// Duration returns the recorded wall-clock time.
func (r Result) Duration() time.Duration { return time.Duration(r.DurationNS) }

// Env records the environment a run happened in.
type Env struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	CPUs   int    `json:"cpus"`
	Go     string `json:"go"`
	Host   string `json:"host,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Tree is the Git tree object of Commit.
	Tree string `json:"tree,omitempty"`
	// Dirty records whether tracked or untracked source files differed
	// from Commit when the run started. Strict validation rejects true.
	Dirty      bool              `json:"dirty"`
	CLIs       map[string]string `json:"clis"`
	RecordedAt time.Time         `json:"recorded_at"`
}

// Doc is a complete recorded run: schema version, environment, and results.
type Doc struct {
	SchemaVersion int      `json:"schema_version"`
	Env           Env      `json:"env"`
	Results       []Result `json:"results"`
}

// WriteJSON writes the doc as formatted JSON.
func (d Doc) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ParseDoc reads a doc written by WriteJSON.
func ParseDoc(data []byte) (Doc, error) {
	var d Doc
	if err := json.Unmarshal(data, &d); err != nil {
		return Doc{}, fmt.Errorf("decode bench doc: %w", err)
	}
	return d, nil
}

// Summary aggregates the iterations of one scenario.
type Summary struct {
	Backend  string
	Library  string
	Image    string
	Scenario string

	Iterations int
	Median     time.Duration
	Min        time.Duration
	Max        time.Duration

	// MedianSubprocesses is the median spawn count across iterations;
	// zero when the scenario does not count subprocesses.
	MedianSubprocesses int64
}

// Summarize groups results by (backend, library, image, scenario) and
// aggregates each group's durations.
func Summarize(results []Result) []Summary {
	groups := map[string][]Result{}
	var keys []string
	for _, r := range results {
		k := r.Backend + "\x00" + r.Library + "\x00" + r.Image + "\x00" + r.Scenario
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Strings(keys)

	out := make([]Summary, 0, len(keys))
	for _, k := range keys {
		rs := groups[k]
		s := Summary{
			Backend:    rs[0].Backend,
			Library:    rs[0].Library,
			Image:      rs[0].Image,
			Scenario:   rs[0].Scenario,
			Iterations: len(rs),
		}
		durations := make([]int64, 0, len(rs))
		subprocesses := make([]int64, 0, len(rs))
		for _, r := range rs {
			durations = append(durations, r.DurationNS)
			subprocesses = append(subprocesses, r.Subprocesses)
			if s.Min == 0 || r.Duration() < s.Min {
				s.Min = r.Duration()
			}
			if r.Duration() > s.Max {
				s.Max = r.Duration()
			}
		}
		s.Median = time.Duration(median(durations))
		s.MedianSubprocesses = median(subprocesses)
		out = append(out, s)
	}
	return out
}

// median returns the middle element of a sorted copy; for an even
// number of elements it rounds up to the upper middle.
func median(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// Table renders summaries as a fixed-width human-readable table.
func Table(summaries []Summary) string {
	const pattern = "%-8s %-18s %-18s %-16s %4s %10s %10s %10s %8s\n"
	var b strings.Builder
	fmt.Fprintf(&b, pattern, "BACKEND", "LIBRARY", "IMAGE", "SCENARIO", "N", "MEDIAN", "MIN", "MAX", "SPAWN")
	for _, s := range summaries {
		fmt.Fprintf(&b, pattern,
			s.Backend, s.Library, s.Image, s.Scenario,
			fmt.Sprint(s.Iterations),
			s.Median.Round(time.Millisecond),
			s.Min.Round(time.Millisecond),
			s.Max.Round(time.Millisecond),
			fmt.Sprint(s.MedianSubprocesses),
		)
	}
	return b.String()
}
