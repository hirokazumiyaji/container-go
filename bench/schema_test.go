package bench

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
		if result.ImageDigest == "" || result.Commit == "" || result.Iterations < 1 {
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
	if doc.Results[0].ImageDigest != "" || doc.Results[0].Iterations != 0 || doc.Env.Commit != "" || doc.SchemaVersion != 0 {
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
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "docs", "benchmarks.md"))
	if err != nil {
		t.Fatalf("read benchmark documentation: %v", err)
	}
	rows := parseBaselineRows(t, data)
	if len(rows) != len(ScenarioNames()) {
		t.Fatalf("baseline has %d scenario rows, want %d", len(rows), len(ScenarioNames()))
	}
	for scenario, row := range rows {
		if _, ok := ScenarioPolicyForKey(row.Backend, row.Library, scenario); !ok {
			t.Errorf("baseline has unknown scenario key %s/%s/%s", row.Backend, row.Library, scenario)
		}
	}
	for _, policy := range ScenarioPolicies() {
		row, ok := rows[policy.Name]
		if !ok {
			t.Errorf("baseline is missing scenario %q", policy.Name)
			continue
		}
		if row.Iterations != policy.Iterations {
			t.Errorf("baseline %q iterations = %d, want %d", policy.Name, row.Iterations, policy.Iterations)
		}
	}
	for _, required := range []string{RedisImage, NginxImage, TestcontainersRyukImage, "schema_version", "env.tree", "docker.client", "docker.server", "apple.service", "ryuk_image", "cache_state"} {
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
	Backend    string
	Library    string
	Iterations int
}

func parseBaselineRows(t *testing.T, data []byte) map[string]baselineRow {
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
	rows := make(map[string]baselineRow)
	for _, line := range lines[start+2:] {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 4 {
			t.Fatalf("malformed baseline row: %q", line)
		}
		backend := strings.TrimSpace(cells[0])
		library := strings.TrimSpace(cells[1])
		scenario := strings.TrimSpace(cells[2])
		iterations, err := strconv.Atoi(strings.TrimSpace(cells[3]))
		if err != nil {
			t.Fatalf("baseline %q iterations: %v", scenario, err)
		}
		if _, exists := rows[scenario]; exists {
			t.Errorf("baseline has duplicate scenario %q", scenario)
		}
		rows[scenario] = baselineRow{Backend: backend, Library: library, Iterations: iterations}
	}
	return rows
}
