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
	rows := make(map[string]int)
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
		scenario := strings.TrimSpace(cells[2])
		iterations, err := strconv.Atoi(strings.TrimSpace(cells[3]))
		if err != nil {
			t.Fatalf("baseline %q iterations: %v", scenario, err)
		}
		if _, exists := rows[scenario]; exists {
			t.Errorf("baseline has duplicate scenario %q", scenario)
		}
		rows[scenario] = iterations
	}

	if len(rows) != len(bench.ScenarioNames()) {
		t.Fatalf("baseline has %d scenario rows, want %d", len(rows), len(bench.ScenarioNames()))
	}
	policies := make(map[string]bench.ScenarioPolicy)
	for _, policy := range bench.ScenarioPolicies() {
		policies[policy.Name] = policy
	}
	for scenario := range rows {
		if _, ok := policies[scenario]; !ok {
			t.Errorf("baseline has unknown scenario %q", scenario)
		}
	}
	for _, image := range []string{bench.RedisImage, bench.NginxImage} {
		if !bytes.Contains(data, []byte(image)) {
			t.Errorf("benchmark documentation does not pin %q", image)
		}
		if bench.ImageDigest(image) == "" {
			t.Errorf("benchmark image %q is not pinned", image)
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
	for _, policy := range bench.ScenarioPolicies() {
		got, ok := rows[policy.Name]
		if !ok {
			t.Errorf("baseline is missing scenario %q", policy.Name)
			continue
		}
		if got != policy.Iterations {
			t.Errorf("baseline %q iterations = %d, want %d", policy.Name, got, policy.Iterations)
		}
	}
}
