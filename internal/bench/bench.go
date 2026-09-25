// Package bench defines the shared measurement schema for the
// benchmark harness. The root module's integration tests and the
// separate bench module (testcontainers-go comparison) both record
// results in this format so later optimization issues can compare
// numbers across runs.
package bench

import (
	"bytes"
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
const CurrentSchemaVersion = 4

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
	// ImageDigest is retained as the expected content digest for Image.
	// ExpectedImageDigest and ObservedImageDigest make the provenance
	// distinction explicit in newly recorded documents.
	ImageDigest         string `json:"image_digest"`
	ExpectedImageDigest string `json:"expected_image_digest"`
	ObservedImageDigest string `json:"observed_image_digest"`
	ObservedImageID     string `json:"observed_image_id,omitempty"`
	// WorkloadCacheState records whether the workload image was absent or
	// present at the instant the timed region started. It is required for
	// every result and is independent of the testcontainers Ryuk state.
	WorkloadCacheState string `json:"workload_cache_state"`
	// RyukImage is the immutable reaper image used by a
	// testcontainers-go scenario. It is empty for container-go scenarios.
	RyukImage string `json:"ryuk_image,omitempty"`
	// RyukImageDigest is the content digest of RyukImage.
	RyukImageDigest string `json:"ryuk_image_digest,omitempty"`
	// CacheState is the testcontainers Ryuk cache state. It is "cold" or
	// "warm" for tc/session-init and empty for every other scenario.
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
	Dirty bool              `json:"dirty"`
	CLIs  map[string]string `json:"clis"`
	// ReaperSessionID is the actual generated testcontainers session used
	// by a testcontainers result. It is empty for container-go-only docs.
	ReaperSessionID string `json:"reaper_session_id,omitempty"`
	// Docker provenance fields describe the effective CLI endpoint and
	// daemon selected for Docker results.
	DockerEndpoint   string    `json:"docker_endpoint,omitempty"`
	DockerContext    string    `json:"docker_context,omitempty"`
	DockerDaemonID   string    `json:"docker_daemon_id,omitempty"`
	DockerDaemonOS   string    `json:"docker_daemon_os,omitempty"`
	DockerDaemonArch string    `json:"docker_daemon_arch,omitempty"`
	RecordedAt       time.Time `json:"recorded_at"`

	// metadataPresent and dirtyPresent preserve JSON field presence while
	// keeping Dirty source-compatible as a bool. They are intentionally
	// not serialized.
	metadataPresent bool
	dirtyPresent    bool
}

// UnmarshalJSON records whether env.dirty was present. A plain bool cannot
// distinguish an omitted field from an explicit false, which matters for the
// strict schema contract.
func (e *Env) UnmarshalJSON(data []byte) error {
	type plainEnv Env
	var decoded plainEnv
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := validateJSONBoolField(fields, "dirty"); err != nil {
		return fmt.Errorf("env.%w", err)
	}
	*e = Env(decoded)
	e.metadataPresent = true
	_, e.dirtyPresent = fields["dirty"]
	return nil
}

func validateJSONBoolField(fields map[string]json.RawMessage, name string) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	value := bytes.TrimSpace(raw)
	if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
		return fmt.Errorf("%s must be a JSON boolean, got %s", name, string(value))
	}
	return nil
}

// NormalizeEnv trims command/version metadata and canonicalizes its CLI map
// without changing identity fields that validation must reject when they are
// malformed.
func NormalizeEnv(env Env) Env {
	env.OS = strings.TrimSpace(env.OS)
	env.Arch = strings.TrimSpace(env.Arch)
	env.Go = strings.TrimSpace(env.Go)
	env.Host = strings.TrimSpace(env.Host)
	env.Commit = strings.TrimSpace(env.Commit)
	env.Tree = strings.TrimSpace(env.Tree)
	env.ReaperSessionID = strings.TrimSpace(env.ReaperSessionID)
	env.DockerEndpoint = strings.TrimSpace(env.DockerEndpoint)
	env.DockerContext = strings.TrimSpace(env.DockerContext)
	env.DockerDaemonID = strings.TrimSpace(env.DockerDaemonID)
	env.DockerDaemonOS = strings.TrimSpace(env.DockerDaemonOS)
	env.DockerDaemonArch = strings.TrimSpace(env.DockerDaemonArch)
	if !env.RecordedAt.IsZero() {
		env.RecordedAt = env.RecordedAt.UTC()
	}
	if env.CLIs != nil {
		clis := make(map[string]string, len(env.CLIs))
		for key, value := range env.CLIs {
			clis[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		env.CLIs = clis
	}
	return env
}

// NormalizeDoc returns a metadata-normalized copy suitable for writing or
// comparison. It does not make an invalid identity valid.
func NormalizeDoc(doc Doc) Doc {
	doc.Env = NormalizeEnv(doc.Env)
	return doc
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
	return enc.Encode(NormalizeDoc(d))
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
	Backend            string
	Library            string
	Image              string
	Scenario           string
	WorkloadCacheState string
	RyukCacheState     string

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
		k := r.Backend + "\x00" + r.Library + "\x00" + r.Image + "\x00" + r.Scenario + "\x00" + r.WorkloadCacheState + "\x00" + r.CacheState
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
			Backend:            rs[0].Backend,
			Library:            rs[0].Library,
			Image:              rs[0].Image,
			Scenario:           rs[0].Scenario,
			WorkloadCacheState: rs[0].WorkloadCacheState,
			RyukCacheState:     rs[0].CacheState,
			Iterations:         len(rs),
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
	const pattern = "%-8s %-18s %-18s %-16s %-8s %-8s %4s %10s %10s %10s %8s\n"
	var b strings.Builder
	fmt.Fprintf(&b, pattern, "BACKEND", "LIBRARY", "IMAGE", "SCENARIO", "WORKLOAD", "RYUK", "N", "MEDIAN", "MIN", "MAX", "SPAWN")
	for _, s := range summaries {
		fmt.Fprintf(&b, pattern,
			s.Backend, s.Library, s.Image, s.Scenario,
			s.WorkloadCacheState, s.RyukCacheState,
			fmt.Sprint(s.Iterations),
			s.Median.Round(time.Millisecond),
			s.Min.Round(time.Millisecond),
			s.Max.Round(time.Millisecond),
			fmt.Sprint(s.MedianSubprocesses),
		)
	}
	return b.String()
}
