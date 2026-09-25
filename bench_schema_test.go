package container

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	bench "github.com/hirokazumiyaji/container-go/internal/bench"
)

func TestBenchmarkDocumentationMatchesScenarioPolicy(t *testing.T) {
	data := readRepositoryFile(t, filepath.Join("docs", "benchmarks.md"))
	rows := parseBenchmarkRows(t, data)
	keys := bench.ScenarioPolicyKeys()
	if len(rows) != len(keys) {
		t.Fatalf("baseline has %d scenario keys, want %d", len(rows), len(keys))
	}
	for key := range rows {
		if _, ok := bench.ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario); !ok {
			t.Errorf("baseline has unknown scenario key %s/%s/%s", key.Backend, key.Library, key.Scenario)
		}
	}
	for _, key := range keys {
		row, ok := rows[key]
		if !ok {
			t.Errorf("baseline is missing scenario key %s/%s/%s", key.Backend, key.Library, key.Scenario)
			continue
		}
		policy, _ := bench.ScenarioPolicyForKey(key.Backend, key.Library, key.Scenario)
		if row.iterations != policy.Iterations {
			t.Errorf("baseline %s/%s/%s iterations = %d, want %d", key.Backend, key.Library, key.Scenario, row.iterations, policy.Iterations)
		}
	}
	for _, required := range []string{
		bench.RedisImage,
		bench.NginxImage,
		bench.TestcontainersRyukImage,
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

type benchmarkRow struct {
	iterations int
}

func parseBenchmarkRows(t *testing.T, data []byte) map[bench.ScenarioKey]benchmarkRow {
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
	rows := make(map[bench.ScenarioKey]benchmarkRow)
	for _, line := range lines[start+2:] {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 4 {
			t.Fatalf("malformed baseline row: %q", line)
		}
		key := bench.ScenarioKey{
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
		rows[key] = benchmarkRow{iterations: iterations}
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
