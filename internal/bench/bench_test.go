package bench

import (
	"bytes"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func sampleResults() []Result {
	run := func(scenario string, it int, d time.Duration, spawns int64) Result {
		policy, ok := ScenarioPolicyFor(scenario)
		if !ok {
			panic("unknown test scenario: " + scenario)
		}
		return Result{
			Backend:      "docker",
			Library:      LibraryContainerGo,
			Image:        policy.Image,
			ImageDigest:  policy.ImageDigest,
			Scenario:     scenario,
			Iteration:    it,
			Iterations:   policy.Iterations,
			Commit:       testCommit,
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
			Backend:     "docker",
			Library:     LibraryTestcontainersGo,
			Image:       RedisImage,
			ImageDigest: RedisImageDigest,
			Scenario:    "tc/single",
			Iteration:   1,
			Iterations:  DefaultIterations,
			Commit:      testCommit,
			DurationNS:  int64(900 * time.Millisecond),
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
			OS:         "darwin",
			Arch:       "arm64",
			CPUs:       10,
			Go:         "go1.27.0",
			Host:       "bench-host",
			Commit:     testCommit,
			CLIs:       map[string]string{"docker": "29.7.2"},
			RecordedAt: time.Unix(1, 0).UTC(),
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

func completeSampleResults() []Result {
	results := sampleResults()
	for iteration := 3; iteration <= DefaultIterations; iteration++ {
		results = append(results, Result{
			Backend:     "docker",
			Library:     LibraryContainerGo,
			Image:       RedisImage,
			ImageDigest: RedisImageDigest,
			Scenario:    "run/cold",
			Iteration:   iteration,
			Iterations:  DefaultIterations,
			Commit:      testCommit,
			DurationNS:  int64(time.Duration(iteration) * time.Second),
		})
	}
	for iteration := 2; iteration <= DefaultIterations; iteration++ {
		results = append(results, Result{
			Backend:     "docker",
			Library:     LibraryTestcontainersGo,
			Image:       RedisImage,
			ImageDigest: RedisImageDigest,
			Scenario:    "tc/single",
			Iteration:   iteration,
			Iterations:  DefaultIterations,
			Commit:      testCommit,
			DurationNS:  int64(900 * time.Millisecond),
		})
	}
	return results
}

func TestValidateDocChecksReproducibilityMetadata(t *testing.T) {
	doc := Doc{
		Env: Env{
			OS:         "darwin",
			Arch:       "arm64",
			CPUs:       10,
			Go:         "go1.27.0",
			Host:       "bench-host",
			Commit:     testCommit,
			CLIs:       map[string]string{"docker": "29.7.2"},
			RecordedAt: time.Unix(1, 0).UTC(),
		},
		Results: completeSampleResults(),
	}
	if err := ValidateDoc(doc); err != nil {
		t.Fatalf("ValidateDoc: %v", err)
	}

	doc.Results[0].Image = "public.ecr.aws/docker/library/redis:7-alpine"
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted a mutable image tag")
	}
}

func TestPinnedImagesMatchPolicyDigests(t *testing.T) {
	for _, test := range []struct {
		image  string
		digest string
	}{
		{image: RedisImage, digest: RedisImageDigest},
		{image: NginxImage, digest: NginxImageDigest},
	} {
		if got := ImageDigest(test.image); got != test.digest {
			t.Errorf("ImageDigest(%q) = %q, want %q", test.image, got, test.digest)
		}
	}
}

func TestScenarioPolicyIncludesSessionInitSpecialCase(t *testing.T) {
	policy, ok := ScenarioPolicyFor("tc/session-init")
	if !ok {
		t.Fatal("tc/session-init policy is missing")
	}
	if policy.Iterations != SessionInitIterations {
		t.Fatalf("tc/session-init iterations = %d, want %d", policy.Iterations, SessionInitIterations)
	}
	for _, name := range ScenarioNames() {
		policy, ok := ScenarioPolicyFor(name)
		if !ok {
			t.Fatalf("policy for %q is missing", name)
		}
		want := DefaultIterations
		if name == "tc/session-init" {
			want = SessionInitIterations
		}
		if policy.Iterations != want {
			t.Errorf("%s iterations = %d, want %d", name, policy.Iterations, want)
		}
	}
}

func TestCurrentCommitHonorsExplicitOverride(t *testing.T) {
	t.Setenv("CONTAINERGO_BENCH_COMMIT", testCommit)
	commit, err := CurrentCommit()
	if err != nil {
		t.Fatalf("CurrentCommit: %v", err)
	}
	if commit != testCommit {
		t.Fatalf("commit = %q, want %q", commit, testCommit)
	}
}

func TestValidateScenarioSetChecksAllIterations(t *testing.T) {
	var results []Result
	for _, policy := range ScenarioPolicies() {
		for iteration := 1; iteration <= policy.Iterations; iteration++ {
			results = append(results, Result{
				Backend:     "docker",
				Library:     LibraryContainerGo,
				Image:       policy.Image,
				ImageDigest: policy.ImageDigest,
				Scenario:    policy.Name,
				Iteration:   iteration,
				Iterations:  policy.Iterations,
				Commit:      testCommit,
				DurationNS:  1,
			})
		}
	}
	if err := ValidateScenarioSet(results); err != nil {
		t.Fatalf("ValidateScenarioSet: %v", err)
	}

	for i, result := range results {
		if result.Scenario != "tc/session-init" {
			continue
		}
		results[i].Iterations = DefaultIterations
		break
	}
	if err := ValidateScenarioSet(results); err == nil {
		t.Fatal("ValidateScenarioSet accepted the wrong session-init iteration policy")
	}
}

func TestCompareDocsRejectsMetadataChanges(t *testing.T) {
	baseline := Doc{Env: Env{Commit: testCommit}, Results: sampleResults()}
	candidate := baseline
	candidate.Results = append([]Result(nil), baseline.Results...)
	if err := CompareDocs(baseline, candidate); err != nil {
		t.Fatalf("same documents: %v", err)
	}

	candidate.Results[0].ImageDigest = NginxImageDigest
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed image digest")
	}

	candidate = baseline
	candidate.Results = append([]Result(nil), baseline.Results...)
	candidate.Results[0].Image = "public.ecr.aws/docker/library/redis:7-alpine"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a mutable image tag")
	}

	candidate = baseline
	candidate.Results = append([]Result(nil), baseline.Results...)
	candidate.Env.Commit = "fedcba9876543210fedcba9876543210fedcba98"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed commit")
	}

	candidate = baseline
	candidate.Results = append([]Result(nil), baseline.Results...)
	candidate.Env.Go = "go1.26.0"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed Go environment")
	}
}

func TestTableRendersAllColumns(t *testing.T) {
	table := Table(Summarize(sampleResults()))
	for _, want := range []string{"BACKEND", "docker", LibraryContainerGo, RedisImage, "run/warm", "300ms", "3"} {
		if !bytes.Contains([]byte(table), []byte(want)) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}
}
