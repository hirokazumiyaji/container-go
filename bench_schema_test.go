package container

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	bench "github.com/hirokazumiyaji/container-go/internal/bench"
)

func TestBenchmarkDocumentationMatchesScenarioPolicy(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "docs", "benchmarks.md"))
	if err != nil {
		t.Fatalf("read benchmark documentation: %v", err)
	}

	rows := parseBenchmarkRows(t, data)
	if len(rows) != len(bench.ScenarioNames()) {
		t.Fatalf("baseline has %d scenario rows, want %d", len(rows), len(bench.ScenarioNames()))
	}
	for scenario, row := range rows {
		if _, ok := bench.ScenarioPolicyForKey(row.backend, row.library, scenario); !ok {
			t.Errorf("baseline has unknown scenario key %s/%s/%s", row.backend, row.library, scenario)
		}
	}
	for _, policy := range bench.ScenarioPolicies() {
		row, ok := rows[policy.Name]
		if !ok {
			t.Errorf("baseline is missing scenario %q", policy.Name)
			continue
		}
		if row.iterations != policy.Iterations {
			t.Errorf("baseline %q iterations = %d, want %d", policy.Name, row.iterations, policy.Iterations)
		}
	}
	for _, required := range []string{bench.RedisImage, bench.NginxImage, bench.TestcontainersRyukImage, "schema_version", "env.tree", "docker.client", "docker.server", "apple.service", "ryuk_image", "cache_state"} {
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

type benchmarkRow struct {
	backend    string
	library    string
	iterations int
}

func parseBenchmarkRows(t *testing.T, data []byte) map[string]benchmarkRow {
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
	rows := make(map[string]benchmarkRow)
	for _, line := range lines[start+2:] {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 4 {
			t.Fatalf("malformed baseline row: %q", line)
		}
		scenario := strings.TrimSpace(cells[2])
		iterations, err := strconv.Atoi(strings.TrimSpace(cells[3]))
		if err != nil {
			t.Fatalf("baseline %q iterations: %v", scenario, err)
		}
		if _, exists := rows[scenario]; exists {
			t.Errorf("baseline has duplicate scenario %q", scenario)
		}
		rows[scenario] = benchmarkRow{
			backend:    strings.TrimSpace(cells[0]),
			library:    strings.TrimSpace(cells[1]),
			iterations: iterations,
		}
	}
	return rows
}
