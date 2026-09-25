//go:build integration

package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
	"github.com/hirokazumiyaji/container-go/wait"
	tc "github.com/testcontainers/testcontainers-go"
	tcwait "github.com/testcontainers/testcontainers-go/wait"
)

// The wall-clock scenarios compare container-go against
// testcontainers-go under identical conditions: same images, same
// readiness probe (listening port), same iteration count. container-go
// runs on both backends; testcontainers-go targets the Docker Engine
// API. Results use the shared schema; see docs/benchmarks.md.

const (
	redisImage = ibench.PinnedRedisImage
	nginxImage = ibench.PinnedNginxImage
)

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}

// record appends one timed iteration to the doc. cacheState is used only by
// tc/session-init; the other testcontainers scenarios inherit the same
// pinned Ryuk image but have no separate cache-state dimension.
func record(doc *Doc, backend, library, image, name string, iteration int, elapsed time.Duration, cacheState string) {
	policy, ok := ibench.ScenarioPolicyForKey(backend, library, name)
	if !ok {
		panic("unknown benchmark scenario key: " + backend + "/" + library + "/" + name)
	}
	doc.Results = append(doc.Results, Result{
		Backend:         backend,
		Library:         library,
		Image:           image,
		ImageDigest:     policy.ImageDigest,
		RyukImage:       policy.RyukImage,
		RyukImageDigest: policy.RyukImageDigest,
		CacheState:      cacheState,
		Scenario:        name,
		Iteration:       iteration,
		Iterations:      policy.Iterations,
		Commit:          doc.Env.Commit,
		DurationNS:      int64(elapsed),
	})
}

// runScenario runs fn iterations times and records each Run→ready
// duration. prep runs before the timer; cleanup runs after it, so
// image setup/teardown and container termination stay out of the
// measurement.
func runScenario(t *testing.T, doc *Doc, backend, library, image, name string, prep func(*testing.T), fn func(*testing.T) (cleanup func(), err error)) {
	t.Helper()
	policy, ok := ibench.ScenarioPolicyForKey(backend, library, name)
	if !ok {
		t.Fatalf("no benchmark policy for scenario %q", name)
	}
	for i := 1; i <= policy.Iterations; i++ {
		if prep != nil {
			prep(t)
		}
		start := time.Now()
		cleanup, err := fn(t)
		elapsed := time.Since(start)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			t.Fatalf("%s iteration %d: %v", name, i, err)
		}
		record(doc, backend, library, image, name, i, elapsed, "")
		if cleanup != nil {
			cleanup()
		}
	}
}

// containerGoStart starts one container and returns after it is ready.
// Callers terminate outside the timed region.
func containerGoStart(t *testing.T, image, port string) (*container.Container, error) {
	t.Helper()
	return container.Run(context.Background(), image,
		container.WithExposedPorts(port),
		container.WithWaitStrategy(wait.ForListeningPort(port)),
	)
}

func terminateCleanup(t *testing.T, containers ...*container.Container) func() {
	t.Helper()
	return func() {
		for _, ctr := range containers {
			if ctr == nil {
				continue
			}
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Logf("terminate: %v", err)
			}
		}
	}
}

func benchEnv(t *testing.T, b ibench.Backend) Env {
	t.Helper()
	source, err := ibench.RequireCleanSource()
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
	return Env{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUs:       runtime.NumCPU(),
		Go:         runtime.Version(),
		Host:       host,
		Commit:     source.Commit,
		Tree:       source.Tree,
		Dirty:      source.Dirty,
		CLIs:       versions,
		RecordedAt: time.Now().UTC(),
	}
}

func writeDoc(t *testing.T, name string, doc Doc) string {
	t.Helper()
	dir := "results"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create results dir: %v", err)
	}
	path := filepath.Join(dir, name+"-"+time.Now().Format("20060102-150405")+".json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create results file: %v", err)
	}
	defer f.Close()
	if err := WriteJSON(f, doc); err != nil {
		t.Fatalf("write results: %v", err)
	}
	return path
}

// TestIntegrationBenchContainerGo measures container-go's Run→ready
// wall-clock for cold, warm, multi, and parallel scenarios on both
// backends.
func TestIntegrationBenchContainerGo(t *testing.T) {
	for _, b := range []ibench.Backend{ibench.DockerBackend(), ibench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			// Pin the backend: the public API selects the engine from
			// CONTAINERGO_BACKEND (or the OS default).
			t.Setenv("CONTAINERGO_BACKEND", b.Name)
			doc := Doc{SchemaVersion: ibench.CurrentSchemaVersion, Env: benchEnv(t, b)}

			// Cold: remove the image so the run includes the pull.
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/cold",
				func(t *testing.T) { b.EnsureImageAbsent(t, redisImage) },
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, redisImage, "6379/tcp")
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/warm", nil,
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, redisImage, "6379/tcp")
					return terminateCleanup(t, ctr), err
				})
			b.EnsureImage(t, nginxImage)
			runScenario(t, &doc, b.Name, LibraryContainerGo, nginxImage, "run/warm-nginx", nil,
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, nginxImage, "80/tcp")
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/no-wait", nil,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage)
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/forlog", nil,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage,
						container.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/forexec", nil,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage,
						container.WithWaitStrategy(wait.ForExec([]string{"redis-cli", "ping"})))
					return terminateCleanup(t, ctr), err
				})

			// Multi: five sequential containers in one process.
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/multi-5", nil,
				func(t *testing.T) (func(), error) {
					var containers []*container.Container
					for range 5 {
						ctr, err := containerGoStart(t, redisImage, "6379/tcp")
						if err != nil {
							return terminateCleanup(t, containers...), err
						}
						containers = append(containers, ctr)
					}
					return terminateCleanup(t, containers...), nil
				})

			// Parallel: eight concurrent starts; the value is the
			// wall-clock until all eight are ready, excluding the
			// termination of the containers.
			const n = 8
			parallelPolicy, ok := ibench.ScenarioPolicyForKey(b.Name, LibraryContainerGo, "run/parallel-8")
			if !ok {
				t.Fatal("no benchmark policy for run/parallel-8")
			}
			for i := 1; i <= parallelPolicy.Iterations; i++ {
				errs := make([]error, n)
				var (
					mu         sync.Mutex
					containers []*container.Container
				)
				var ready sync.WaitGroup
				start := time.Now()
				for idx := range n {
					ready.Add(1)
					go func() {
						defer ready.Done()
						ctr, err := container.Run(context.Background(), redisImage,
							container.WithExposedPorts("6379/tcp"),
							container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
						)
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
				for _, err := range errs {
					if err != nil {
						t.Fatalf("run/parallel-8 iteration %d: %v", i, err)
					}
				}
				record(&doc, b.Name, LibraryContainerGo, redisImage, "run/parallel-8", i, elapsed, "")
				for _, ctr := range containers {
					if err := ctr.Terminate(context.Background()); err != nil {
						t.Logf("run/parallel-8 iteration %d: terminate: %v", i, err)
					}
				}
			}

			if err := ibench.ValidateDoc(doc); err != nil {
				t.Fatalf("validate benchmark result: %v", err)
			}
			path := writeDoc(t, b.Name, doc)
			t.Log("\n" + Table(Summarize(doc.Results)))
			t.Logf("results written to %s", path)
		})
	}
}

// tcRequest is the shared testcontainers-go request: redis with the
// same listening-port readiness probe the container-go scenarios use.
func tcRequest() tc.GenericContainerRequest {
	return tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        redisImage,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   tcwait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	}
}

func tcTerminateCleanup(t *testing.T, containers ...tc.Container) func() {
	t.Helper()
	return func() {
		for _, ctr := range containers {
			if ctr == nil {
				continue
			}
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Logf("terminate: %v", err)
			}
		}
	}
}

// prepareTestcontainersRyuk establishes the local image state used by the
// session-init measurement. The mutable tag is accepted only after its
// resolved repository digest is verified against the policy pin.
func prepareTestcontainersRyuk(t *testing.T, b ibench.Backend) string {
	t.Helper()
	if tc.ReaperDefaultImage != ibench.TestcontainersRyukTag {
		t.Fatalf("testcontainers Ryuk default = %q, want %q", tc.ReaperDefaultImage, ibench.TestcontainersRyukTag)
	}
	if b.ImageDigest == nil || b.TagImage == nil {
		t.Fatal("Docker image digest/tag operations are required for the Ryuk benchmark")
	}

	mode := os.Getenv("CONTAINERGO_BENCH_RYUK_CACHE")
	if mode == "" {
		mode = "auto"
	}
	pinnedCached, err := b.ImageExists(ibench.TestcontainersRyukImage)
	if err != nil {
		t.Fatalf("inspect pinned Ryuk cache: %v", err)
	}
	switch mode {
	case "auto":
		if pinnedCached {
			if err := verifyTestcontainersRyukDigest(b, ibench.TestcontainersRyukImage); err != nil {
				t.Fatal(err)
			}
			if err := b.TagImage(ibench.TestcontainersRyukImage, ibench.TestcontainersRyukTag); err != nil {
				t.Fatalf("tag cached pinned Ryuk image: %v", err)
			}
			if err := verifyTestcontainersRyuk(b); err != nil {
				t.Fatal(err)
			}
			return ibench.CacheStateWarm
		}
		b.EnsureImageAbsent(t, ibench.TestcontainersRyukTag)
		return ibench.CacheStateCold
	case "warm":
		b.EnsureImage(t, ibench.TestcontainersRyukImage)
		if err := b.TagImage(ibench.TestcontainersRyukImage, ibench.TestcontainersRyukTag); err != nil {
			t.Fatalf("tag pinned Ryuk image: %v", err)
		}
		if err := verifyTestcontainersRyuk(b); err != nil {
			t.Fatal(err)
		}
		return ibench.CacheStateWarm
	case "cold":
		b.EnsureImageAbsent(t, ibench.TestcontainersRyukTag)
		b.EnsureImageAbsent(t, ibench.TestcontainersRyukImage)
		return ibench.CacheStateCold
	default:
		t.Fatalf("CONTAINERGO_BENCH_RYUK_CACHE=%q, want auto, warm, or cold", mode)
		return ""
	}
}

func verifyTestcontainersRyuk(b ibench.Backend) error {
	return verifyTestcontainersRyukDigest(b, ibench.TestcontainersRyukTag)
}

func verifyTestcontainersRyukDigest(b ibench.Backend, image string) error {
	digest, err := b.ImageDigest(image)
	if err != nil {
		return fmt.Errorf("verify Ryuk image: %w", err)
	}
	if digest != ibench.TestcontainersRyukImageDigest {
		return fmt.Errorf("Ryuk image digest = %q, want %q", digest, ibench.TestcontainersRyukImageDigest)
	}
	return nil
}

// TestIntegrationBenchTestcontainers measures testcontainers-go under
// the same conditions. The first iteration includes the session
// initialization (starting and connecting the Ryuk sidecar container),
// recorded separately as tc/session-init; steady-state values follow.
func TestIntegrationBenchTestcontainers(t *testing.T) {
	requireDocker(t)
	doc := Doc{SchemaVersion: ibench.CurrentSchemaVersion, Env: benchEnv(t, ibench.DockerBackend())}
	cacheState := prepareTestcontainersRyuk(t, ibench.DockerBackend())

	// Session init plus the first container: recorded as its own
	// scenario so steady-state numbers stay comparable.
	start := time.Now()
	ctr, err := tc.GenericContainer(context.Background(), tcRequest())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("session-init container: %v", err)
	}
	if err := verifyTestcontainersRyuk(ibench.DockerBackend()); err != nil {
		_ = ctr.Terminate(context.Background())
		t.Fatalf("session-init Ryuk provenance: %v", err)
	}
	record(&doc, "docker", LibraryTestcontainersGo, redisImage, "tc/session-init", 1, elapsed, cacheState)
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	runScenario(t, &doc, "docker", LibraryTestcontainersGo, redisImage, "tc/single", nil,
		func(t *testing.T) (func(), error) {
			ctr, err := tc.GenericContainer(context.Background(), tcRequest())
			return tcTerminateCleanup(t, ctr), err
		})

	// Multi: five sequential containers in one process; the session
	// initialization was already paid above.
	runScenario(t, &doc, "docker", LibraryTestcontainersGo, redisImage, "tc/multi-5", nil,
		func(t *testing.T) (func(), error) {
			var containers []tc.Container
			for range 5 {
				ctr, err := tc.GenericContainer(context.Background(), tcRequest())
				if err != nil {
					return tcTerminateCleanup(t, containers...), err
				}
				containers = append(containers, ctr)
			}
			return tcTerminateCleanup(t, containers...), nil
		})

	if err := ibench.ValidateDoc(doc); err != nil {
		t.Fatalf("validate benchmark result: %v", err)
	}
	path := writeDoc(t, "docker-tc", doc)
	t.Log("\n" + Table(Summarize(doc.Results)))
	t.Logf("results written to %s", path)
}
