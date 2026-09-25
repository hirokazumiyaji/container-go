package bench

import (
	"bytes"
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
	}
	for _, required := range []string{
		RedisImage,
		NginxImage,
		TestcontainersRyukImage,
		"schema_version",
		"workload_cache_state",
		"env.tree",
		"env.dirty",
		"docker.client",
		"docker.server",
		"apple.client",
		"apple.service",
		"ryuk_image",
		"cache_state",
		"reaper_session_id",
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

type baselineRow struct {
	Iterations int
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
		if len(cells) < 4 {
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
		rows[key] = baselineRow{Iterations: iterations}
	}
	return rows
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
