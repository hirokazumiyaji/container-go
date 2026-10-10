package bench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testCommit        = "0123456789abcdef0123456789abcdef01234567"
	testTree          = "89abcdef0123456789abcdef0123456789abcdef"
	testReaperSession = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func sampleResults() []Result {
	run := func(scenario string, it int, d time.Duration, spawns int64) Result {
		policy, ok := ScenarioPolicyForKey("docker", LibraryContainerGo, scenario)
		if !ok {
			panic("unknown test scenario: " + scenario)
		}
		return Result{
			Backend:             "docker",
			Library:             LibraryContainerGo,
			Image:               policy.Image,
			ImageDigest:         policy.ImageDigest,
			ExpectedImageDigest: policy.ImageDigest,
			ObservedImageDigest: policy.ImageDigest,
			ObservedImageID:     "sha256:workload",
			WorkloadCacheState:  policy.WorkloadCacheStates[0],
			Scenario:            scenario,
			Iteration:           it,
			Iterations:          policy.Iterations,
			Commit:              testCommit,
			DurationNS:          int64(d),
			Subprocesses:        spawns,
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
			Backend:             "docker",
			Library:             LibraryTestcontainersGo,
			Image:               RedisImage,
			ImageDigest:         RedisImageDigest,
			ExpectedImageDigest: RedisImageDigest,
			ObservedImageDigest: RedisImageDigest,
			ObservedImageID:     "sha256:workload",
			RyukImage:           TestcontainersRyukImage,
			RyukImageDigest:     TestcontainersRyukImageDigest,
			Scenario:            "tc/single",
			Iteration:           1,
			Iterations:          DefaultIterations,
			Commit:              testCommit,
			DurationNS:          int64(900 * time.Millisecond),
		},
	}
}

func completeResults(commit string) []Result {
	var results []Result
	for _, policy := range ScenarioPoliciesFor("docker", LibraryContainerGo) {
		for iteration := 1; iteration <= policy.Iterations; iteration++ {
			results = append(results, Result{
				Backend:             "docker",
				Library:             LibraryContainerGo,
				Image:               policy.Image,
				ImageDigest:         policy.ImageDigest,
				ExpectedImageDigest: policy.ImageDigest,
				ObservedImageDigest: policy.ImageDigest,
				ObservedImageID:     "sha256:workload",
				WorkloadCacheState:  policy.WorkloadCacheStates[0],
				Scenario:            policy.Name,
				Iteration:           iteration,
				Iterations:          policy.Iterations,
				Commit:              commit,
				DurationNS:          1,
			})
		}
	}
	for _, policy := range ScenarioPoliciesFor("docker", LibraryTestcontainersGo) {
		for iteration := 1; iteration <= policy.Iterations; iteration++ {
			cacheState := ""
			if policy.Name == "tc/session-init" {
				cacheState = CacheStateWarm
			}
			results = append(results, Result{
				Backend:             "docker",
				Library:             LibraryTestcontainersGo,
				Image:               policy.Image,
				ImageDigest:         policy.ImageDigest,
				ExpectedImageDigest: policy.ImageDigest,
				ObservedImageDigest: policy.ImageDigest,
				ObservedImageID:     "sha256:workload",
				WorkloadCacheState:  policy.WorkloadCacheStates[0],
				RyukImage:           policy.RyukImage,
				RyukImageDigest:     policy.RyukImageDigest,
				CacheState:          cacheState,
				Scenario:            policy.Name,
				Iteration:           iteration,
				Iterations:          policy.Iterations,
				Commit:              commit,
				DurationNS:          1,
			})
		}
	}
	return results
}

func testEnv(commit string) Env {
	return Env{
		OS:               "darwin",
		Arch:             "arm64",
		CPUs:             10,
		Go:               "go1.27.0",
		Host:             "bench-host",
		Commit:           commit,
		Tree:             testTree,
		ReaperSessionID:  testReaperSession,
		DockerEndpoint:   "unix:///var/run/docker.sock",
		DockerContext:    "default",
		DockerDaemonID:   "daemon-id",
		DockerDaemonOS:   "linux",
		DockerDaemonArch: "arm64",
		CLIs: map[string]string{
			DockerClientVersionKey: "29.8.0",
			DockerServerVersionKey: "29.8.0",
		},
		RecordedAt: time.Unix(1, 0).UTC(),
	}
}

func completeDoc(commit string) Doc {
	return Doc{SchemaVersion: CurrentSchemaVersion, Env: testEnv(commit), Results: completeResults(commit)}
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
	doc := completeDoc(testCommit)
	var buf bytes.Buffer
	if err := doc.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	parsed, err := ParseDoc(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	if parsed.SchemaVersion != CurrentSchemaVersion || parsed.Env.Tree != testTree {
		t.Errorf("schema/source = %d/%q", parsed.SchemaVersion, parsed.Env.Tree)
	}
	if parsed.Env.CLIs[DockerClientVersionKey] != "29.8.0" {
		t.Errorf("env = %+v", parsed.Env)
	}
	if len(parsed.Results) != len(doc.Results) {
		t.Fatalf("results = %d, want %d", len(parsed.Results), len(doc.Results))
	}
	for i, r := range parsed.Results {
		if r != doc.Results[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, r, doc.Results[i])
		}
	}
}

func TestParseDocRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseDoc([]byte("not json")); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

func TestValidateDocRequiresExplicitDirtyField(t *testing.T) {
	doc := completeDoc(testCommit)
	var buf bytes.Buffer
	if err := doc.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	env := raw["env"].(map[string]any)
	delete(env, "dirty")
	withoutDirty, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDoc(withoutDirty)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDoc(parsed); err == nil || !strings.Contains(err.Error(), "env.dirty") {
		t.Fatalf("ValidateDoc error = %v, want missing env.dirty", err)
	}
}

func TestJSONDirtyRejectsNullAndNonBooleanValues(t *testing.T) {
	doc := completeDoc(testCommit)
	var encoded bytes.Buffer
	if err := doc.WriteJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	for _, dirty := range []string{"null", `"false"`, "1"} {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(encoded.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw["env"], &env); err != nil {
			t.Fatal(err)
		}
		env["dirty"] = json.RawMessage(dirty)
		envData, marshalErr := json.Marshal(env)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		raw["env"] = envData
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseDoc(data); err == nil {
			t.Errorf("Env dirty=%s was accepted", dirty)
		}
	}
}

func TestNormalizeEnvCanonicalizesMetadata(t *testing.T) {
	env := testEnv(testCommit)
	env.OS = " darwin "
	env.CLIs[DockerClientVersionKey] = " 29.8.0\n"
	env.RecordedAt = time.Unix(1, 0).In(time.FixedZone("offset", 3600))
	normalized := NormalizeEnv(env)
	if normalized.OS != "darwin" || normalized.CLIs[DockerClientVersionKey] != "29.8.0" || normalized.RecordedAt.Location() != time.UTC {
		t.Fatalf("normalized env = %+v", normalized)
	}
}

func TestValidateDocRejectsUnknownCLIMetadata(t *testing.T) {
	doc := completeDoc(testCommit)
	doc.Env.CLIs["docker.other"] = "1"
	if err := ValidateDoc(doc); err == nil || !strings.Contains(err.Error(), "not a recognized") {
		t.Fatalf("ValidateDoc error = %v, want unknown CLI key", err)
	}
}

func TestValidateDocChecksReproducibilityMetadata(t *testing.T) {
	doc := completeDoc(testCommit)
	if err := ValidateDoc(doc); err != nil {
		t.Fatalf("ValidateDoc: %v", err)
	}

	doc.Results[0].Image = "public.ecr.aws/docker/library/redis:7-alpine"
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted a mutable image tag")
	}

	doc = completeDoc(testCommit)
	doc.Env.Dirty = true
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted dirty source")
	}

	doc = completeDoc(testCommit)
	delete(doc.Env.CLIs, DockerClientVersionKey)
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted missing Docker client version")
	}

	doc = completeDoc(testCommit)
	for i := range doc.Results {
		if doc.Results[i].Scenario == "tc/session-init" {
			doc.Results[i].CacheState = "unknown"
			break
		}
	}
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted an invalid Ryuk cache state")
	}

	doc = completeDoc(testCommit)
	doc.Results[0].WorkloadCacheState = CacheStateWarm
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted the wrong workload cache state")
	}

	doc = completeDoc(testCommit)
	doc.Results[0].ObservedImageDigest = NginxImageDigest
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted a mismatched observed image digest")
	}

	doc = completeDoc(testCommit)
	doc.Results[0].ExpectedImageDigest = ""
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted a missing expected image digest")
	}

	doc = completeDoc(testCommit)
	doc.Results[0].ObservedImageID = ""
	if err := ValidateDoc(doc); err == nil {
		t.Fatal("ValidateDoc accepted a missing Docker observed image ID")
	}
}

func TestPinnedImagesMatchPolicyDigests(t *testing.T) {
	for _, test := range []struct {
		image  string
		digest string
	}{
		{image: RedisImage, digest: RedisImageDigest},
		{image: NginxImage, digest: NginxImageDigest},
		{image: TestcontainersRyukImage, digest: TestcontainersRyukImageDigest},
	} {
		if got := ImageDigest(test.image); got != test.digest {
			t.Errorf("ImageDigest(%q) = %q, want %q", test.image, got, test.digest)
		}
	}
}

func TestScenarioPolicyIncludesIdentityAndSessionInitSpecialCase(t *testing.T) {
	policy, ok := ScenarioPolicyForKey("docker", LibraryTestcontainersGo, "tc/session-init")
	if !ok {
		t.Fatal("tc/session-init policy is missing")
	}
	if policy.Iterations != SessionInitIterations || len(policy.CacheStates) != 2 || len(policy.WorkloadCacheStates) != 1 || policy.WorkloadCacheStates[0] != CacheStateWarm {
		t.Fatalf("tc/session-init policy = %+v", policy)
	}
	if _, ok := ScenarioPolicyForKey("apple", LibraryTestcontainersGo, "tc/single"); ok {
		t.Fatal("testcontainers policy was accepted for Apple")
	}
	if _, ok := ScenarioPolicyForKey("docker", LibraryContainerGo, "tc/single"); ok {
		t.Fatal("tc policy was accepted for container-go")
	}
	if _, ok := ScenarioPolicyForKey("apple", LibraryContainerGo, "run/warm"); !ok {
		t.Fatal("run policy was not bound to Apple")
	}
	if len(ScenarioPoliciesFor("docker", LibraryContainerGo)) != 8 {
		t.Fatal("Docker container-go policy set is incomplete")
	}
	if len(ScenarioPoliciesFor("apple", LibraryContainerGo)) != 8 {
		t.Fatal("Apple container-go policy set is incomplete")
	}
	if len(ScenarioPoliciesFor("docker", LibraryTestcontainersGo)) != 3 {
		t.Fatal("Docker testcontainers policy set is incomplete")
	}
	if len(ScenarioPolicyKeys()) != 19 {
		t.Fatalf("complete scenario key set = %d, want 19", len(ScenarioPolicyKeys()))
	}
}

func fakeGit(t *testing.T, dirty bool) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "rev-parse --verify")
    case "$3" in
      "HEAD^{commit}") echo 0123456789abcdef0123456789abcdef01234567 ;;
      "0123456789abcdef0123456789abcdef01234567^{tree}") echo 89abcdef0123456789abcdef0123456789abcdef ;;
      *) exit 1 ;;
    esac
    ;;
  "status --porcelain=v1")
    if [ "` + map[bool]string{true: "1", false: "0"}[dirty] + `" = 1 ]; then echo " M source.go"; fi
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCurrentSourceValidatesOverrideAndRecordsTreeAndDirty(t *testing.T) {
	fakeGit(t, false)
	t.Setenv("CONTAINERGO_BENCH_COMMIT", testCommit)
	source, err := RequireCleanSource()
	if err != nil {
		t.Fatalf("RequireCleanSource: %v", err)
	}
	if source.Commit != testCommit || source.Tree != testTree || source.Dirty {
		t.Fatalf("source = %+v", source)
	}

	t.Setenv("CONTAINERGO_BENCH_COMMIT", testCommit+"0")
	if _, err := CurrentSource(); err == nil {
		t.Fatal("CurrentSource accepted an invalid override")
	}

	fakeGit(t, true)
	t.Setenv("CONTAINERGO_BENCH_COMMIT", "")
	source, err = CurrentSource()
	if err != nil || !source.Dirty {
		t.Fatalf("dirty source = %+v, err=%v", source, err)
	}
	if _, err := RequireCleanSource(); err == nil {
		t.Fatal("RequireCleanSource accepted dirty source")
	}
	if _, err := CurrentCommit(); err == nil {
		t.Fatal("CurrentCommit accepted dirty source")
	}
}

func TestCurrentCommitRejectsMismatchedOverride(t *testing.T) {
	fakeGit(t, false)
	t.Setenv("CONTAINERGO_BENCH_COMMIT", "fedcba9876543210fedcba9876543210fedcba98")
	if _, err := CurrentCommit(); err == nil {
		t.Fatal("CurrentCommit accepted a commit other than HEAD")
	}
}

func TestValidateScenarioSetChecksAllIterationsAndIdentities(t *testing.T) {
	results := completeResults(testCommit)
	if err := ValidateScenarioSet(results); err != nil {
		t.Fatalf("ValidateScenarioSet: %v", err)
	}

	results = results[:len(results)-1]
	if err := ValidateScenarioSet(results); err == nil {
		t.Fatal("ValidateScenarioSet accepted a missing iteration")
	}

	results = completeResults(testCommit)
	for i := range results {
		if results[i].Scenario == "tc/session-init" {
			results[i].Iterations = DefaultIterations
			break
		}
	}
	if err := ValidateScenarioSet(results); err == nil {
		t.Fatal("ValidateScenarioSet accepted the wrong session-init iteration policy")
	}
}

func TestCompareDocsValidatesAndAllowsSourceRevisionChanges(t *testing.T) {
	baseline := completeDoc(testCommit)
	candidate := completeDoc(testCommit)
	if err := CompareDocs(baseline, candidate); err != nil {
		t.Fatalf("same documents: %v", err)
	}

	const candidateCommit = "fedcba9876543210fedcba9876543210fedcba98"
	const candidateTree = "98abcdef0123456789abcdef0123456789abcdef"
	candidate = completeDoc(candidateCommit)
	candidate.Env.Tree = candidateTree
	if err := CompareDocs(baseline, candidate); err != nil {
		t.Fatalf("source revision change was rejected: %v", err)
	}

	candidate = completeDoc(candidateCommit)
	candidate.Env.Go = "go1.26.0"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed Go environment")
	}

	for name, mutate := range map[string]func(*Env){
		"endpoint":    func(env *Env) { env.DockerEndpoint = "tcp://other.example:2375" },
		"context":     func(env *Env) { env.DockerContext = "other" },
		"daemon ID":   func(env *Env) { env.DockerDaemonID = "other-daemon" },
		"daemon OS":   func(env *Env) { env.DockerDaemonOS = "windows" },
		"daemon arch": func(env *Env) { env.DockerDaemonArch = "amd64" },
	} {
		candidate = completeDoc(candidateCommit)
		mutate(&candidate.Env)
		if err := CompareDocs(baseline, candidate); err == nil {
			t.Errorf("CompareDocs accepted changed Docker %s provenance", name)
		}
	}

	candidate = completeDoc(candidateCommit)
	candidate.Results[0].ObservedImageDigest = NginxImageDigest
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted changed observed image digest")
	}
	candidate = completeDoc(candidateCommit)
	candidate.Results[0].ObservedImageID = "sha256:other"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted changed observed image ID")
	}

	candidate = completeDoc(candidateCommit)
	candidate.Results[0].Image = "public.ecr.aws/docker/library/redis:7-alpine"
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a mutable image tag")
	}

	candidate = completeDoc(candidateCommit)
	for i := range candidate.Results {
		if candidate.Results[i].Scenario == "tc/session-init" {
			candidate.Results[i].CacheState = CacheStateCold
			break
		}
	}
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed Ryuk cache state")
	}

	candidate = completeDoc(candidateCommit)
	candidate.Results[0].WorkloadCacheState = CacheStateWarm
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a changed workload cache state")
	}

	candidate = completeDoc(candidateCommit)
	candidate.Results = candidate.Results[:len(candidate.Results)-1]
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a missing scenario iteration")
	}

	candidate = completeDoc(candidateCommit)
	candidate.SchemaVersion = 0
	if err := CompareDocs(baseline, candidate); err == nil {
		t.Fatal("CompareDocs accepted a missing schema version")
	}
}

func TestTableRendersAllColumns(t *testing.T) {
	table := Table(Summarize(sampleResults()))
	for _, want := range []string{"BACKEND", "WORKLOAD", "RYUK", "docker", LibraryContainerGo, RedisImage, "run/warm", "300ms", "3"} {
		if !bytes.Contains([]byte(table), []byte(want)) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}
}
