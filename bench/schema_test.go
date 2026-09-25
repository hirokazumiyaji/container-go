package bench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestFixtureMatchesSchema validates the committed fixture against the strict
// result schema without requiring a backend.
func TestFixtureMatchesSchema(t *testing.T) {
	data := readFixture(t, "result-doc.json")
	doc, err := ParseDoc(data)
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	if err := ValidateDoc(doc); err != nil {
		t.Fatalf("ValidateDoc: %v", err)
	}
	if doc.SchemaVersion != CurrentSchemaVersion || doc.Env.OS == "" || doc.Env.Arch == "" || doc.Env.Go == "" || doc.Env.Commit == "" || doc.Env.Tree == "" {
		t.Errorf("env/schema incomplete: version=%d env=%+v", doc.SchemaVersion, doc.Env)
	}
	if doc.Env.CLIs[DockerClientVersionKey] == "" || doc.Env.CLIs[DockerServerVersionKey] == "" {
		t.Errorf("Docker client/server metadata incomplete: %+v", doc.Env.CLIs)
	}
	if len(doc.Results) == 0 {
		t.Fatal("fixture has no results")
	}
	for i, result := range doc.Results {
		if result.Backend == "" || result.Library == "" || result.Image == "" || result.Scenario == "" {
			t.Errorf("result[%d] missing identity fields: %+v", i, result)
		}
		if result.ImageDigest == "" || result.WorkloadCacheState == "" || result.Commit == "" || result.Iterations < 1 {
			t.Errorf("result[%d] missing reproducibility fields: %+v", i, result)
		}
	}
	if Table(Summarize(doc.Results)) == "" {
		t.Fatal("table output is empty")
	}
}

func TestParseDocKeepsLegacyDocumentsParseable(t *testing.T) {
	legacy := []byte(`{
		"env":{"os":"darwin","arch":"arm64","cpus":10,"go":"go1.23.0","clis":{"docker":"1"},"recorded_at":"2026-01-01T00:00:00Z"},
		"results":[{"backend":"docker","library":"container-go","image":"redis:7-alpine","scenario":"run/warm","iteration":1,"duration_ns":1,"subprocesses":2}]
	}`)
	doc, err := ParseDoc(legacy)
	if err != nil {
		t.Fatalf("ParseDoc legacy document: %v", err)
	}
	if len(doc.Results) != 1 || doc.Results[0].Iteration != 1 {
		t.Fatalf("legacy results = %+v", doc.Results)
	}
	if doc.Results[0].ImageDigest != "" || doc.Results[0].WorkloadCacheState != "" || doc.Results[0].Iterations != 0 || doc.Env.Commit != "" || doc.SchemaVersion != 0 {
		t.Fatalf("legacy metadata unexpectedly synthesized: %+v", doc)
	}
}

func TestPinnedBenchmarkImages(t *testing.T) {
	for _, image := range []string{RedisImage, NginxImage, TestcontainersRyukImage} {
		if got := ImageDigest(image); got == "" {
			t.Errorf("image %q is not pinned", image)
		}
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("image %q does not contain a sha256 digest", image)
		}
	}
}

func TestBenchmarkDocsMatchScenarioPolicy(t *testing.T) {
	data := readRepositoryFile(t, filepath.Join("..", "docs", "benchmarks.md"))
	rows := parseBaselineRows(t, data)
	keys := ScenarioPolicyKeys()
	if len(rows) != len(keys) {
		t.Fatalf("baseline has %d scenario keys, want %d", len(rows), len(keys))
	}
	for key := range rows {
		if _, ok := ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario); !ok {
			t.Errorf("baseline has unknown scenario key %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
	}
	for _, key := range keys {
		row, ok := rows[key]
		if !ok {
			t.Errorf("baseline is missing scenario key %s/%s/%s", key.Backend, key.Library, key.Scenario)
			continue
		}
		policy, _ := ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario)
		if row.Iterations != policy.Iterations {
			t.Errorf("baseline %s/%s/%s iterations = %d, want %d", key.Backend, key.Library, key.Scenario, row.Iterations, policy.Iterations)
		}
		ryukImage, ryukDigest := row.RyukImage, row.RyukDigest
		if policy.RyukImage == "" && ryukImage == "-" && ryukDigest == "-" {
			ryukImage, ryukDigest = "", ""
		}
		if row.Image != policy.Image || row.ImageDigest != policy.ImageDigest || ryukImage != policy.RyukImage || ryukDigest != policy.RyukImageDigest || row.WorkloadCache != policy.WorkloadCacheStates[0] {
			t.Errorf("baseline %s/%s/%s image/cache provenance does not match policy: row=%+v", key.Backend, key.Library, key.Scenario, row)
		}
		if !validDocumentedCacheStates(row.RyukCache, policy.CacheStates) {
			t.Errorf("baseline %s/%s/%s Ryuk cache provenance = %q, want %v", key.Backend, key.Library, key.Scenario, row.RyukCache, policy.CacheStates)
		}
	}
	for _, required := range []string{
		RedisImage,
		NginxImage,
		TestcontainersRyukImage,
		"schema_version",
		"workload_cache_state",
		"expected_image_digest",
		"observed_image_digest",
		"observed_image_id",
		"env.tree",
		"env.dirty",
		"docker.client",
		"docker.server",
		"docker_endpoint",
		"docker_context",
		"docker_daemon_id",
		"docker_daemon_os",
		"docker_daemon_arch",
		"apple.client",
		"apple.service",
		"ryuk_image",
		"ryuk_image_digest",
		"synthetic",
		"cache_state",
		"reaper_session_id",
		"TESTCONTAINERS_CONFIG",
		"ryuk.container.image",
		"Java-properties",
		"does not traverse",
		"CI workflows",
	} {
		if !bytes.Contains(data, []byte(required)) {
			t.Errorf("benchmark documentation does not describe %q", required)
		}
	}
	for _, mutable := range []string{
		"public.ecr.aws/docker/library/redis:7-alpine",
		"public.ecr.aws/docker/library/nginx:alpine",
	} {
		if bytes.Contains(data, []byte(mutable)) {
			t.Errorf("benchmark documentation still uses mutable image %q", mutable)
		}
	}
}

func TestBaselineProvenanceFixtureMatchesCompletePolicy(t *testing.T) {
	data := readRepositoryFile(t, filepath.Join("..", "testdata", "benchmark-baseline.json"))
	var baseline BaselineProvenance
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatalf("decode baseline provenance: %v", err)
	}
	if err := ValidateBaselineProvenance(baseline); err != nil {
		t.Fatalf("validate baseline provenance: %v", err)
	}
}

func TestBenchmarkDocsMatchBaselineFixture(t *testing.T) {
	rows := parseBaselineRows(t, readRepositoryFile(t, filepath.Join("..", "docs", "benchmarks.md")))
	var baseline BaselineProvenance
	if err := json.Unmarshal(readRepositoryFile(t, filepath.Join("..", "testdata", "benchmark-baseline.json")), &baseline); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range baseline.Scenarios {
		key := ScenarioKey{Backend: scenario.Backend, Library: scenario.Library, Scenario: scenario.Scenario}
		row, ok := rows[key]
		if !ok {
			t.Errorf("documentation is missing baseline scenario %s/%s/%s", key.Backend, key.Library, key.Scenario)
			continue
		}
		if row.Iterations != scenario.Iterations || row.Image != scenario.Image || row.ImageDigest != scenario.ImageDigest {
			t.Errorf("documentation row %s/%s/%s = %+v, baseline = %+v", key.Backend, key.Library, key.Scenario, row, scenario)
		}
		if len(scenario.WorkloadCacheStates) != 1 || row.WorkloadCache != scenario.WorkloadCacheStates[0] {
			t.Errorf("documentation workload cache for %s/%s/%s = %q, baseline = %v", key.Backend, key.Library, key.Scenario, row.WorkloadCache, scenario.WorkloadCacheStates)
		}
		wantRyuk := "-"
		if len(scenario.RyukCacheStates) == 2 && scenario.RyukCacheStates[0] == CacheStateCold && scenario.RyukCacheStates[1] == CacheStateWarm {
			wantRyuk = "cold/warm"
		} else if len(scenario.RyukCacheStates) == 1 {
			wantRyuk = scenario.RyukCacheStates[0]
		}
		if row.RyukCache != wantRyuk {
			t.Errorf("documentation Ryuk cache for %s/%s/%s = %q, baseline = %q", key.Backend, key.Library, key.Scenario, row.RyukCache, wantRyuk)
		}
	}
}

type baselineRow struct {
	Iterations    int
	Image         string
	ImageDigest   string
	RyukImage     string
	RyukDigest    string
	WorkloadCache string
	RyukCache     string
}

func parseBaselineRows(t *testing.T, data []byte) map[ScenarioKey]baselineRow {
	t.Helper()
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "| Backend | Library") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("baseline table header not found")
	}
	rows := make(map[ScenarioKey]baselineRow)
	for _, line := range lines[start+2:] {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 10 {
			t.Fatalf("malformed baseline row: %q", line)
		}
		key := ScenarioKey{
			Backend:  strings.TrimSpace(cells[0]),
			Library:  strings.TrimSpace(cells[1]),
			Scenario: strings.TrimSpace(cells[2]),
		}
		iterations, err := strconv.Atoi(strings.TrimSpace(cells[3]))
		if err != nil {
			t.Fatalf("baseline %s/%s/%s iterations: %v", key.Backend, key.Library, key.Scenario, err)
		}
		if _, exists := rows[key]; exists {
			t.Errorf("baseline has duplicate scenario key %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
		rows[key] = baselineRow{
			Iterations:    iterations,
			Image:         strings.TrimSpace(cells[4]),
			ImageDigest:   strings.TrimSpace(cells[5]),
			RyukImage:     strings.TrimSpace(cells[6]),
			RyukDigest:    strings.TrimSpace(cells[7]),
			WorkloadCache: strings.TrimSpace(cells[8]),
			RyukCache:     strings.TrimSpace(cells[9]),
		}
	}
	return rows
}

func validDocumentedCacheStates(value string, allowed []string) bool {
	if len(allowed) == 0 {
		return value == "-"
	}
	if len(allowed) == 2 && value == "cold/warm" {
		return allowed[0] == CacheStateCold && allowed[1] == CacheStateWarm
	}
	for _, state := range allowed {
		if value == state {
			return true
		}
	}
	return false
}

func readRepositoryFile(t *testing.T, relative string) []byte {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		candidate := filepath.Join(dir, relative)
		if data, err := os.ReadFile(candidate); err == nil {
			return data
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("repository file %q not found from working directory", relative)
	return nil
}
