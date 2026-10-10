package container

import (
	"bytes"
	"encoding/json"
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
		ryukImage, ryukDigest := row.ryukImage, row.ryukDigest
		if policy.RyukImage == "" && ryukImage == "-" && ryukDigest == "-" {
			ryukImage, ryukDigest = "", ""
		}
		if row.image != policy.Image || row.imageDigest != policy.ImageDigest || ryukImage != policy.RyukImage || ryukDigest != policy.RyukImageDigest || row.workloadCache != policy.WorkloadCacheStates[0] {
			t.Errorf("baseline %s/%s/%s image/cache provenance does not match policy: row=%+v", key.Backend, key.Library, key.Scenario, row)
		}
		if scenarioCacheState(row.ryukCache, policy.CacheStates) == "" {
			t.Errorf("baseline %s/%s/%s Ryuk cache provenance = %q, want one of %v", key.Backend, key.Library, key.Scenario, row.ryukCache, policy.CacheStates)
		}
	}
	for _, required := range []string{
		bench.RedisImage,
		bench.NginxImage,
		bench.TestcontainersRyukImage,
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

func TestBenchmarkBaselineProvenanceIsComplete(t *testing.T) {
	data := readRepositoryFile(t, filepath.Join("testdata", "benchmark-baseline.json"))
	var baseline bench.BaselineProvenance
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatalf("decode benchmark baseline provenance: %v", err)
	}
	if err := bench.ValidateBaselineProvenance(baseline); err != nil {
		t.Fatalf("validate benchmark baseline provenance: %v", err)
	}
}

func TestBenchmarkDocumentationMatchesBaselineFixture(t *testing.T) {
	docs := parseBenchmarkRows(t, readRepositoryFile(t, filepath.Join("docs", "benchmarks.md")))
	var baseline bench.BaselineProvenance
	if err := json.Unmarshal(readRepositoryFile(t, filepath.Join("testdata", "benchmark-baseline.json")), &baseline); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range baseline.Scenarios {
		key := bench.ScenarioKey{Backend: scenario.Backend, Library: scenario.Library, Scenario: scenario.Scenario}
		row, ok := docs[key]
		if !ok {
			t.Errorf("documentation is missing baseline scenario %s/%s/%s", key.Backend, key.Library, key.Scenario)
			continue
		}
		if row.iterations != scenario.Iterations || row.image != scenario.Image || row.imageDigest != scenario.ImageDigest {
			t.Errorf("documentation row %s/%s/%s = %+v, baseline = %+v", key.Backend, key.Library, key.Scenario, row, scenario)
		}
		if len(scenario.WorkloadCacheStates) != 1 || row.workloadCache != scenario.WorkloadCacheStates[0] {
			t.Errorf("documentation workload cache for %s/%s/%s = %q, baseline = %v", key.Backend, key.Library, key.Scenario, row.workloadCache, scenario.WorkloadCacheStates)
		}
		wantRyuk := "-"
		if len(scenario.RyukCacheStates) == 2 && scenario.RyukCacheStates[0] == bench.CacheStateCold && scenario.RyukCacheStates[1] == bench.CacheStateWarm {
			wantRyuk = "cold/warm"
		} else if len(scenario.RyukCacheStates) == 1 {
			wantRyuk = scenario.RyukCacheStates[0]
		}
		if row.ryukCache != wantRyuk {
			t.Errorf("documentation Ryuk cache for %s/%s/%s = %q, baseline = %q", key.Backend, key.Library, key.Scenario, row.ryukCache, wantRyuk)
		}
	}
}

type benchmarkRow struct {
	iterations    int
	image         string
	imageDigest   string
	ryukImage     string
	ryukDigest    string
	workloadCache string
	ryukCache     string
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
		if len(cells) < 10 {
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
		rows[key] = benchmarkRow{
			iterations:    iterations,
			image:         strings.TrimSpace(cells[4]),
			imageDigest:   strings.TrimSpace(cells[5]),
			ryukImage:     strings.TrimSpace(cells[6]),
			ryukDigest:    strings.TrimSpace(cells[7]),
			workloadCache: strings.TrimSpace(cells[8]),
			ryukCache:     strings.TrimSpace(cells[9]),
		}
	}
	return rows
}

func scenarioCacheState(value string, allowed []string) string {
	if len(allowed) == 0 {
		if value == "-" {
			return "-"
		}
		return ""
	}
	if len(allowed) == 2 && allowed[0] == bench.CacheStateCold && allowed[1] == bench.CacheStateWarm && value == "cold/warm" {
		return value
	}
	for _, state := range allowed {
		if value == state {
			return value
		}
	}
	return ""
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
