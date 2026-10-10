//go:build integration

package bench

import (
	"context"
	"errors"
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
	redisImage = "public.ecr.aws/docker/library/redis:7-alpine"
	nginxImage = "public.ecr.aws/docker/library/nginx:alpine"
)

func isolateBenchmarkEnv(t *testing.T) {
	t.Helper()
	// Benchmarks own teardown; do not inherit a developer's diagnostic
	// retention setting from the parent integration process.
	t.Setenv("CONTAINERGO_KEEP", "0")
}

func requireDocker(t *testing.T) {
	t.Helper()
	isolateBenchmarkEnv(t)
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

// runScenario runs fn iterations times and records each Run→ready
// duration. prep runs before the timer; cleanup runs after it, so
// image setup/teardown and container termination stay out of the
// measurement.
func runScenario(t *testing.T, doc *Doc, backend, library, image, name string, prep func(*testing.T), fn func(*testing.T) (cleanup func(), err error)) {
	t.Helper()
	for i := 1; i <= iterations; i++ {
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
		record(doc, backend, library, image, name, i, elapsed)
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
	isolateBenchmarkEnv(t)
	for _, b := range []ibench.Backend{ibench.DockerBackend(), ibench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			// Pin the backend: the public API selects the engine from
			// CONTAINERGO_BACKEND (or the OS default).
			t.Setenv("CONTAINERGO_BACKEND", b.Name)
			doc := Doc{Env: benchEnv(b)}

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

			// Multi: five sequential containers in one process.
			runScenario(t, &doc, b.Name, LibraryContainerGo, redisImage, "run/multi-5", nil,
				func(t *testing.T) (func(), error) {
					var containers []*container.Container
					for range 5 {
						ctr, err := containerGoStart(t, redisImage, "6379/tcp")
						if ctr != nil {
							containers = append(containers, ctr)
						}
						if err != nil {
							return terminateCleanup(t, containers...), err
						}
					}
					return terminateCleanup(t, containers...), nil
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
						if ctr != nil {
							mu.Lock()
							containers = append(containers, ctr)
							mu.Unlock()
						}
						if err != nil {
							errs[idx] = err
						}
					}()
				}
				ready.Wait()
				elapsed := time.Since(start)
				// Clean up successful and partial handles before reporting
				// any failed iteration.
				terminateCleanup(t, containers...)()
				for _, err := range errs {
					if err != nil {
						t.Fatalf("run/parallel-8 iteration %d: %v", i, err)
					}
				}
				record(&doc, b.Name, LibraryContainerGo, redisImage, "run/parallel-8", i, elapsed)
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

// runTestcontainersMulti keeps cleanup outside the measured region while
// retaining a partial container returned alongside a failed create/start.
func runTestcontainersMulti(t *testing.T, create func() (tc.Container, error)) (func(), error) {
	var containers []tc.Container
	for range 5 {
		ctr, err := create()
		if ctr != nil {
			containers = append(containers, ctr)
		}
		if err != nil {
			return tcTerminateCleanup(t, containers...), err
		}
	}
	return tcTerminateCleanup(t, containers...), nil
}

type partialTestcontainersContainer struct {
	tc.Container
	terminateCalls int
}

func (c *partialTestcontainersContainer) Terminate(context.Context, ...tc.TerminateOption) error {
	c.terminateCalls++
	return nil
}

func TestRunTestcontainersMultiCleansPartialHandleOnError(t *testing.T) {
	previous := &partialTestcontainersContainer{}
	partial := &partialTestcontainersContainer{}
	createErr := errors.New("create failed")
	calls := 0

	cleanup, err := runTestcontainersMulti(t, func() (tc.Container, error) {
		calls++
		if calls == 1 {
			return previous, nil
		}
		return partial, createErr
	})
	if !errors.Is(err, createErr) {
		t.Fatalf("error = %v, want %v", err, createErr)
	}
	if cleanup == nil {
		t.Fatal("multi scenario returned nil cleanup")
	}
	cleanup()

	if previous.terminateCalls != 1 {
		t.Fatalf("previous terminate calls = %d, want 1", previous.terminateCalls)
	}
	if partial.terminateCalls != 1 {
		t.Fatalf("partial terminate calls = %d, want 1", partial.terminateCalls)
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
	elapsed := time.Since(start)
	if err != nil {
		if ctr != nil {
			if termErr := ctr.Terminate(context.Background()); termErr != nil {
				t.Logf("terminate session-init container: %v", termErr)
			}
		}
		t.Fatalf("session-init container: %v", err)
	}
	record(&doc, "docker", LibraryTestcontainersGo, redisImage, "tc/session-init", 1, elapsed)
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
			return runTestcontainersMulti(t, func() (tc.Container, error) {
				return tc.GenericContainer(context.Background(), tcRequest())
			})
		})

	path := writeDoc(t, "docker-tc", doc)
	t.Log("\n" + Table(Summarize(doc.Results)))
	t.Logf("results written to %s", path)
}
