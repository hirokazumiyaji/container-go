//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/bench"
	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// The counting scenarios measure how many CLI child processes each Run
// shape spawns against a real backend, plus the wall-clock time from
// Run to ready. Results land in the shared schema (internal/bench) so
// the subprocess-reduction issues (#18, #19, #20) can compare before
// and after. The testcontainers-go wall-clock comparison lives in the
// separate bench module; see docs/benchmarks.md.

// benchEngines maps a harness backend onto the internal engine that
// builds the CLI argv vectors.
func benchEngines(b bench.Backend) (engine, bool) {
	switch b.Name {
	case "docker":
		return dockerEngine{}, true
	case "apple":
		return appleEngine{}, true
	default:
		return nil, false
	}
}

// benchPortOptions is the default readiness condition: redis accepting
// connections on 6379.
func benchPortOptions(b bench.Backend, eng engine) func(cli.Runner) []Option {
	return func(r cli.Runner) []Option {
		return []Option{
			withRunner(r),
			withEngine(eng),
			WithExposedPorts("6379/tcp"),
			WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
		}
	}
}

// benchScenario runs one scenario according to the shared iteration policy,
// recording duration and subprocess count per iteration. prep runs before each
// timed iteration (image removal for cold, image ensure for warm).
// Terminate happens after the measurement.
func benchScenario(t *testing.T, doc *bench.Doc, b bench.Backend, image, scenario string, prep func(*testing.T), opts func(cli.Runner) []Option) {
	t.Helper()
	policy, ok := bench.ScenarioPolicyForKey(b.Name, bench.LibraryContainerGo, scenario)
	if !ok {
		t.Fatalf("no benchmark policy for scenario %q", scenario)
	}
	if len(policy.WorkloadCacheStates) == 0 {
		t.Fatalf("benchmark policy %q has no workload cache state", scenario)
	}
	for i := 1; i <= policy.Iterations; i++ {
		if prep != nil {
			prep(t)
		} else {
			b.EnsureImage(t, image)
		}
		r := newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
		start := time.Now()
		ctr, err := Run(context.Background(), image, opts(r)...)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s iteration %d: Run: %v", scenario, i, err)
		}
		spawns := r.count()
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("%s iteration %d: terminate: %v", scenario, i, err)
		}
		identity := observeBenchImage(t, b, image)
		doc.Results = append(doc.Results, bench.Result{
			Backend:             b.Name,
			Library:             bench.LibraryContainerGo,
			Image:               image,
			ImageDigest:         policy.ImageDigest,
			ExpectedImageDigest: policy.ImageDigest,
			ObservedImageDigest: identity.Digest,
			ObservedImageID:     identity.ContentID,
			WorkloadCacheState:  policy.WorkloadCacheStates[0],
			Scenario:            scenario,
			Iteration:           i,
			Iterations:          policy.Iterations,
			Commit:              doc.Env.Commit,
			DurationNS:          int64(elapsed),
			Subprocesses:        spawns,
		})
	}
}

func observeBenchImage(t *testing.T, b bench.Backend, image string) bench.ImageIdentity {
	t.Helper()
	identity, err := b.ObserveImage(image)
	if err != nil {
		t.Fatalf("observe workload image %s: %v", image, err)
	}
	expected := bench.ImageDigest(image)
	if identity.Digest == "" || identity.Digest != expected {
		t.Fatalf("observed workload image %s digest = %q, want %q", image, identity.Digest, expected)
	}
	if b.Name == "docker" && identity.ContentID == "" {
		t.Fatalf("observed Docker workload image %s has no content ID", image)
	}
	return identity
}

// TestIntegrationBenchCounting records Run→ready durations and
// subprocess counts for both backends. It skips cleanly when a backend
// is unavailable.
func TestIntegrationBenchCounting(t *testing.T) {
	for _, b := range []bench.Backend{bench.DockerBackend(), bench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			// Pin the backend for the public Run path as well as the
			// explicitly injected counting runner.
			t.Setenv("CONTAINERGO_BACKEND", b.Name)
			eng, ok := benchEngines(b)
			if !ok {
				t.Fatalf("no engine for backend %s", b.Name)
			}
			image := bench.PinnedRedisImage

			doc := bench.Doc{SchemaVersion: bench.CurrentSchemaVersion, Env: benchEnv(t, b)}
			benchScenario(t, &doc, b, image, "run/cold", func(t *testing.T) {
				b.EnsureImageAbsent(t, image)
			}, benchPortOptions(b, eng))
			benchScenario(t, &doc, b, image, "run/warm", nil, benchPortOptions(b, eng))
			nginxImage := bench.PinnedNginxImage
			benchScenario(t, &doc, b, nginxImage, "run/warm-nginx", nil, func(r cli.Runner) []Option {
				return []Option{
					withRunner(r),
					withEngine(eng),
					WithExposedPorts("80/tcp"),
					WithWaitStrategy(wait.ForListeningPort("80/tcp")),
				}
			})
			benchScenario(t, &doc, b, image, "run/no-wait", nil, func(r cli.Runner) []Option {
				return []Option{withRunner(r), withEngine(eng)}
			})
			benchScenario(t, &doc, b, image, "run/forlog", nil, func(r cli.Runner) []Option {
				return []Option{
					withRunner(r),
					withEngine(eng),
					WithWaitStrategy(wait.ForLog("Ready to accept connections")),
				}
			})
			benchScenario(t, &doc, b, image, "run/forexec", nil, func(r cli.Runner) []Option {
				return []Option{
					withRunner(r),
					withEngine(eng),
					WithWaitStrategy(wait.ForExec([]string{"redis-cli", "ping"})),
				}
			})
			benchMulti(t, &doc, b, eng, image)
			benchParallel(t, &doc, b, eng, image)

			if err := bench.ValidateDoc(doc); err != nil {
				t.Fatalf("validate benchmark result: %v", err)
			}
			path := writeBenchDoc(t, b.Name, doc)
			t.Log("\n" + bench.Table(bench.Summarize(doc.Results)))
			t.Logf("results written to %s", path)
		})
	}
}

// benchMulti measures five sequential containers in one process. Setup and
// termination stay outside the measured interval.
func benchMulti(t *testing.T, doc *bench.Doc, b bench.Backend, eng engine, image string) {
	t.Helper()
	policy, ok := bench.ScenarioPolicyForKey(b.Name, bench.LibraryContainerGo, "run/multi-5")
	if !ok {
		t.Fatal("no benchmark policy for run/multi-5")
	}
	for i := 1; i <= policy.Iterations; i++ {
		b.EnsureImage(t, image)
		r := newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
		var containers []*Container
		start := time.Now()
		for range 5 {
			ctr, err := Run(context.Background(), image, benchPortOptions(b, eng)(r)...)
			if err != nil {
				for _, started := range containers {
					_ = started.Terminate(context.Background())
				}
				t.Fatalf("multi iteration %d: %v", i, err)
			}
			containers = append(containers, ctr)
		}
		elapsed := time.Since(start)
		for _, ctr := range containers {
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Logf("multi iteration %d: terminate: %v", i, err)
			}
		}
		identity := observeBenchImage(t, b, image)
		doc.Results = append(doc.Results, bench.Result{
			Backend:             b.Name,
			Library:             bench.LibraryContainerGo,
			Image:               image,
			ImageDigest:         policy.ImageDigest,
			ExpectedImageDigest: policy.ImageDigest,
			ObservedImageDigest: identity.Digest,
			ObservedImageID:     identity.ContentID,
			WorkloadCacheState:  policy.WorkloadCacheStates[0],
			Scenario:            "run/multi-5",
			Iteration:           i,
			Iterations:          policy.Iterations,
			Commit:              doc.Env.Commit,
			DurationNS:          int64(elapsed),
			Subprocesses:        r.count(),
		})
	}
}

// benchParallel measures the wall-clock time until N containers
// started in parallel are all ready. Individual durations are not
// summed: the scenario reports one elapsed time per iteration.
func benchParallel(t *testing.T, doc *bench.Doc, b bench.Backend, eng engine, image string) {
	t.Helper()
	const n = 8
	b.EnsureImage(t, image)

	policy, ok := bench.ScenarioPolicyForKey(b.Name, bench.LibraryContainerGo, "run/parallel-8")
	if !ok {
		t.Fatal("no benchmark policy for run/parallel-8")
	}
	for i := 1; i <= policy.Iterations; i++ {
		var (
			mu         sync.Mutex
			containers []*Container
			errs       = make([]error, n)
		)
		r := newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
		var ready sync.WaitGroup
		start := time.Now()
		for idx := range n {
			ready.Add(1)
			go func() {
				defer ready.Done()
				ctr, err := Run(context.Background(), image, benchPortOptions(b, eng)(r)...)
				if err != nil {
					errs[idx] = err
					return
				}
				mu.Lock()
				containers = append(containers, ctr)
				mu.Unlock()
			}()
		}
		ready.Wait()
		elapsed := time.Since(start)
		spawns := r.count()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("parallel iteration %d: %v", i, err)
			}
		}
		for _, ctr := range containers {
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Logf("parallel iteration %d: terminate: %v", i, err)
			}
		}
		identity := observeBenchImage(t, b, image)
		doc.Results = append(doc.Results, bench.Result{
			Backend:             b.Name,
			Library:             bench.LibraryContainerGo,
			Image:               image,
			ImageDigest:         policy.ImageDigest,
			ExpectedImageDigest: policy.ImageDigest,
			ObservedImageDigest: identity.Digest,
			ObservedImageID:     identity.ContentID,
			WorkloadCacheState:  policy.WorkloadCacheStates[0],
			Scenario:            "run/parallel-8",
			Iteration:           i,
			Iterations:          policy.Iterations,
			Commit:              doc.Env.Commit,
			DurationNS:          int64(elapsed),
			Subprocesses:        spawns,
		})
	}
}

func benchEnv(t *testing.T, b bench.Backend) bench.Env {
	t.Helper()
	source, err := bench.RequireCleanSource()
	if err != nil {
		t.Fatalf("resolve clean benchmark source: %v", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("resolve benchmark host: %v", err)
	}
	versions, err := b.Versions()
	if err != nil {
		t.Fatalf("record backend versions: %v", err)
	}
	provenance, err := b.CaptureProvenance()
	if err != nil {
		t.Fatalf("record backend provenance: %v", err)
	}
	return bench.Env{
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		CPUs:             runtime.NumCPU(),
		Go:               runtime.Version(),
		Host:             host,
		Commit:           source.Commit,
		Tree:             source.Tree,
		Dirty:            source.Dirty,
		CLIs:             versions,
		DockerEndpoint:   provenance.Endpoint,
		DockerContext:    provenance.Context,
		DockerDaemonID:   provenance.DaemonID,
		DockerDaemonOS:   provenance.DaemonOS,
		DockerDaemonArch: provenance.DaemonArch,
		RecordedAt:       time.Now().UTC(),
	}
}

func writeBenchDoc(t *testing.T, backend string, doc bench.Doc) string {
	t.Helper()
	dir := filepath.Join("bench", "results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create results dir: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("counting-%s-%d.json", backend, time.Now().Unix()))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create results file: %v", err)
	}
	defer f.Close()
	if err := doc.WriteJSON(f); err != nil {
		t.Fatalf("write results: %v", err)
	}
	return path
}
