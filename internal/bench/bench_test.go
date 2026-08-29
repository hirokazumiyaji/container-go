package bench

import (
	"bytes"
	"testing"
	"time"
)

func sampleResults() []Result {
	run := func(scenario string, it int, d time.Duration, spawns int64) Result {
		return Result{
			Backend:      "docker",
			Library:      LibraryContainerGo,
			Image:        "redis:7-alpine",
			Scenario:     scenario,
			Iteration:    it,
			DurationNS:   int64(d),
			Subprocesses: spawns,
		}
	}
	return []Result{
		run("run/warm", 1, 300*time.Millisecond, 3),
		run("run/warm", 2, 100*time.Millisecond, 3),
		run("run/warm", 3, 200*time.Millisecond, 3),
		run("run/warm", 4, 500*time.Millisecond, 4),
		run("run/warm", 5, 400*time.Millisecond, 3),
		run("run/cold", 1, 2*time.Second, 4),
		run("run/cold", 2, 3*time.Second, 4),
		{
			Backend:    "docker",
			Library:    LibraryTestcontainersGo,
			Image:      "redis:7-alpine",
			Scenario:   "tc/single",
			Iteration:  1,
			DurationNS: int64(900 * time.Millisecond),
		},
	}
}

func TestSummarizeGroupsAndAggregates(t *testing.T) {
	summaries := Summarize(sampleResults())
	if len(summaries) != 3 {
		t.Fatalf("summaries = %d, want 3", len(summaries))
	}

	warm := summaries[1]
	if warm.Scenario != "run/warm" || warm.Iterations != 5 {
		t.Fatalf("warm summary = %+v", warm)
	}
	if warm.Median != 300*time.Millisecond {
		t.Errorf("median = %v, want 300ms", warm.Median)
	}
	if warm.Min != 100*time.Millisecond || warm.Max != 500*time.Millisecond {
		t.Errorf("min/max = %v/%v, want 100ms/500ms", warm.Min, warm.Max)
	}
	if warm.MedianSubprocesses != 3 {
		t.Errorf("median subprocesses = %d, want 3", warm.MedianSubprocesses)
	}

	cold := summaries[0]
	if cold.Median != 3*time.Second {
		t.Errorf("cold median = %v, want 3s (upper middle of two)", cold.Median)
	}

	tc := summaries[2]
	if tc.Library != LibraryTestcontainersGo || tc.MedianSubprocesses != 0 {
		t.Errorf("tc summary = %+v", tc)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if got := Summarize(nil); len(got) != 0 {
		t.Errorf("Summarize(nil) = %v, want empty", got)
	}
}

func TestDocJSONRoundTrip(t *testing.T) {
	doc := Doc{
		Env: Env{
			OS:   "darwin",
			Arch: "arm64",
			CPUs: 10,
			Go:   "go1.27.0",
			CLIs: map[string]string{"docker": "29.7.2"},
		},
		Results: sampleResults(),
	}
	var buf bytes.Buffer
	if err := doc.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	parsed, err := ParseDoc(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	if parsed.Env.OS != "darwin" || parsed.Env.CLIs["docker"] != "29.7.2" {
		t.Errorf("env = %+v", parsed.Env)
	}
	if len(parsed.Results) != len(doc.Results) {
		t.Fatalf("results = %d, want %d", len(parsed.Results), len(doc.Results))
	}
	for i, r := range parsed.Results {
		want := doc.Results[i]
		if r != want {
			t.Errorf("result[%d] = %+v, want %+v", i, r, want)
		}
	}
}

func TestParseDocRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseDoc([]byte("not json")); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

func TestTableRendersAllColumns(t *testing.T) {
	table := Table(Summarize(sampleResults()))
	for _, want := range []string{"BACKEND", "docker", LibraryContainerGo, "redis:7-alpine", "run/warm", "300ms", "3"} {
		if !bytes.Contains([]byte(table), []byte(want)) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}
}
