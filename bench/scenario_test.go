//go:build integration

package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

const iterations = 5

const (
	redisImage = "redis:7-alpine"
	nginxImage = "nginx:alpine"
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

// record appends one timed iteration to the doc.
func record(doc *Doc, backend, library, image, name string, iteration int, elapsed time.Duration) {
	doc.Results = append(doc.Results, Result{
		Backend:    backend,
		Library:    library,
		Image:      image,
		Scenario:   name,
		Iteration:  iteration,
		DurationNS: int64(elapsed),
	})
}

// runScenario runs fn iterations times and records each duration.
func runScenario(t *testing.T, doc *Doc, backend, library, image, name string, fn func(t *testing.T) error) {
	t.Helper()
	for i := 1; i <= iterations; i++ {
		start := time.Now()
		if err := fn(t); err != nil {
			t.Fatalf("%s iteration %d: %v", name, i, err)
		}
		record(doc, backend, library, image, name, i, time.Since(start))
	}
}

// containerGoRun starts one container and returns after it is ready.
func containerGoRun(t *testing.T, image, port string) error {
	t.Helper()
	ctr, err := container.Run(context.Background(), image,
		container.WithExposedPorts(port),
		container.WithWaitStrategy(wait.ForListeningPort(port)),
	)
	if err != nil {
		return err
	}
	return ctr.Terminate(context.Background())
}

func benchEnv(b ibench.Backend) Env {
	env := Env{
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
			doc := Doc{Env: benchEnv(b)}

			// Cold: remove the image so the run includes the pull.
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/cold", func(t *testing.T) error {
				if err := b.RemoveImage(redisImage); err != nil {
					t.Logf("remove image: %v", err)
				}
				return containerGoRun(t, redisImage, "6379/tcp")
			})
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/warm", func(t *testing.T) error {
				return containerGoRun(t, redisImage, "6379/tcp")
			})
			b.EnsureImage(t, nginxImage)
			runScenario(t, &doc, b.Name, LibraryContainerGo, nginxImage, "run/warm-nginx", func(t *testing.T) error {
				return containerGoRun(t, nginxImage, "80/tcp")
			})

			// Multi: five sequential containers in one process.
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/multi-5", func(t *testing.T) error {
				for range 5 {
					if err := containerGoRun(t, redisImage, "6379/tcp"); err != nil {
						return err
					}
				}
				return nil
			})

			// Parallel: eight concurrent starts; the value is the
			// wall-clock until all eight are ready, excluding the
			// termination of the containers.
			const n = 8
			for i := 1; i <= iterations; i++ {
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
				record(&doc, b.Name, LibraryContainerGo, redisImage, "run/parallel-8", i, elapsed)
				for _, ctr := range containers {
					if err := ctr.Terminate(context.Background()); err != nil {
						t.Logf("run/parallel-8 iteration %d: terminate: %v", i, err)
					}
				}
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

// TestIntegrationBenchTestcontainers measures testcontainers-go under
// the same conditions. The first iteration includes the session
// initialization (starting and connecting the Ryuk sidecar container),
// recorded separately as tc/session-init; steady-state values follow.
func TestIntegrationBenchTestcontainers(t *testing.T) {
	requireDocker(t)
	doc := Doc{Env: benchEnv(ibench.DockerBackend())}

	// Session init plus the first container: recorded as its own
	// scenario so steady-state numbers stay comparable.
	start := time.Now()
	ctr, err := tc.GenericContainer(context.Background(), tcRequest())
	if err != nil {
		t.Fatalf("session-init container: %v", err)
	}
	record(&doc, "docker", LibraryTestcontainersGo, redisImage, "tc/session-init", 1, time.Since(start))
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	runScenario(t, &doc, "docker", LibraryTestcontainersGo, redisImage, "tc/single", func(t *testing.T) error {
		ctr, err := tc.GenericContainer(context.Background(), tcRequest())
		if err != nil {
			return err
		}
		return ctr.Terminate(context.Background())
	})

	// Multi: five sequential containers in one process; the session
	// initialization was already paid above.
	runScenario(t, &doc, "docker", LibraryTestcontainersGo, redisImage, "tc/multi-5", func(t *testing.T) error {
		for range 5 {
			ctr, err := tc.GenericContainer(context.Background(), tcRequest())
			if err != nil {
				return err
			}
			if err := ctr.Terminate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})

	path := writeDoc(t, "docker-tc", doc)
	t.Log("\n" + Table(Summarize(doc.Results)))
	t.Logf("results written to %s", path)
}
