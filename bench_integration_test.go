//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

const benchIterations = 5

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

// benchScenario runs one scenario benchIterations times, recording
// duration and subprocess count per iteration. prep runs before each
// timed iteration (image removal for cold, image ensure for warm).
// Terminate happens after the measurement.
func benchScenario(t *testing.T, doc *bench.Doc, b bench.Backend, image, scenario string, prep func(*testing.T), opts func(cli.Runner) []Option) {
	t.Helper()
	for i := 1; i <= benchIterations; i++ {
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
		doc.Results = append(doc.Results, bench.Result{
			Backend:      b.Name,
			Library:      bench.LibraryContainerGo,
			Image:        image,
			Scenario:     scenario,
			Iteration:    i,
			DurationNS:   int64(elapsed),
			Subprocesses: spawns,
		})
	}
}

// TestIntegrationBenchCounting records Run→ready durations and
// subprocess counts for both backends. It skips cleanly when a backend
// is unavailable.
func TestIntegrationBenchCounting(t *testing.T) {
	for _, b := range []bench.Backend{bench.DockerBackend(), bench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			eng, ok := benchEngines(b)
			if !ok {
				t.Fatalf("no engine for backend %s", b.Name)
			}
			image := integrationRedis

			doc := bench.Doc{Env: benchEnv(b)}
			benchScenario(t, &doc, b, image, "run/cold", func(t *testing.T) {
				b.EnsureImageAbsent(t, image)
			}, benchPortOptions(b, eng))
			benchScenario(t, &doc, b, image, "run/warm", nil, benchPortOptions(b, eng))
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
			benchParallel(t, &doc, b, eng, image)

			path := writeBenchDoc(t, b.Name, doc)
			t.Log("\n" + bench.Table(bench.Summarize(doc.Results)))
			t.Logf("results written to %s", path)
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

	for i := 1; i <= benchIterations; i++ {
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
		doc.Results = append(doc.Results, bench.Result{
			Backend:      b.Name,
			Library:      bench.LibraryContainerGo,
			Image:        image,
			Scenario:     "run/parallel-8",
			Iteration:    i,
			DurationNS:   int64(elapsed),
			Subprocesses: spawns,
		})
	}
}

func benchEnv(b bench.Backend) bench.Env {
	env := bench.Env{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUs:       runtime.NumCPU(),
		Go:         runtime.Version(),
		CLIs:       map[string]string{},
		RecordedAt: time.Now().UTC(),
	}
	out, err := exec.Command(b.Bin, b.VersionArgs...).Output()
	if err == nil {
		env.CLIs[b.Name] = strings.TrimSpace(string(out))
	}
	return env
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
