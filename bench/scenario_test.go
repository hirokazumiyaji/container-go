//go:build integration

package bench

import (
	"context"
	"errors"
	"fmt"
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

const (
	redisImage = ibench.PinnedRedisImage
	nginxImage = ibench.PinnedNginxImage
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
	if selected := os.Getenv("CONTAINERGO_BACKEND"); selected != "" {
		if selected != "docker" && selected != "apple" {
			t.Fatalf("invalid CONTAINERGO_BACKEND=%q: valid values are \"apple\" and \"docker\"", selected)
		}
		if selected != "docker" {
			t.Skipf("CONTAINERGO_BACKEND=%s; skipping Docker benchmark", selected)
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}

type workloadObservation struct {
	digest string
	id     string
}

func observeWorkloadImage(t *testing.T, backend ibench.Backend, image string) workloadObservation {
	t.Helper()
	identity, err := backend.ObserveImage(image)
	if err != nil {
		t.Fatalf("observe workload image %s: %v", image, err)
	}
	expected := ibench.ImageDigest(image)
	if identity.Digest == "" || identity.Digest != expected {
		t.Fatalf("observed workload image %s digest = %q, want %q", image, identity.Digest, expected)
	}
	if backend.Name == "docker" && identity.ContentID == "" {
		t.Fatalf("observed Docker workload image %s has no content ID", image)
	}
	return workloadObservation{digest: identity.Digest, id: identity.ContentID}
}

// record appends one timed iteration to the doc. cacheState is used only by
// tc/session-init; the other testcontainers scenarios inherit the same
// pinned Ryuk image but have no separate cache-state dimension.
func record(doc *Doc, backend, library, image, name string, iteration int, elapsed time.Duration, cacheState string, observation workloadObservation) {
	policy, ok := ibench.ScenarioPolicyForKey(backend, library, name)
	if !ok {
		panic("unknown benchmark scenario key: " + backend + "/" + library + "/" + name)
	}
	if len(policy.WorkloadCacheStates) == 0 {
		panic("benchmark policy has no workload cache state: " + name)
	}
	doc.Results = append(doc.Results, Result{
		Backend:             backend,
		Library:             library,
		Image:               image,
		ImageDigest:         policy.ImageDigest,
		ExpectedImageDigest: policy.ImageDigest,
		ObservedImageDigest: observation.digest,
		ObservedImageID:     observation.id,
		WorkloadCacheState:  policy.WorkloadCacheStates[0],
		RyukImage:           policy.RyukImage,
		RyukImageDigest:     policy.RyukImageDigest,
		CacheState:          cacheState,
		Scenario:            name,
		Iteration:           iteration,
		Iterations:          policy.Iterations,
		Commit:              doc.Env.Commit,
		DurationNS:          int64(elapsed),
	})
}

// runScenario runs fn iterations times and records each Run→ready
// duration. prep and ensureImage run before the timer; cleanup runs after
// it, so image setup/teardown and container termination stay out of the
// measurement.
func runScenario(t *testing.T, doc *Doc, backend ibench.Backend, library, image, name string, prep, ensureImage func(*testing.T), fn func(*testing.T) (cleanup func(), err error)) {
	t.Helper()
	backendName := backend.Name
	policy, ok := ibench.ScenarioPolicyForKey(backendName, library, name)
	if !ok {
		t.Fatalf("no benchmark policy for scenario %q", name)
	}
	for i := 1; i <= policy.Iterations; i++ {
		if prep != nil {
			prep(t)
		}
		if ensureImage != nil {
			ensureImage(t)
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
		observation := observeWorkloadImage(t, backend, image)
		record(doc, backendName, library, image, name, i, elapsed, "", observation)
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
	provenance, err := b.CaptureProvenance()
	if err != nil {
		t.Fatalf("record backend provenance: %v", err)
	}
	return benchEnvWithProvenance(t, b, provenance)
}

func benchEnvWithProvenance(t *testing.T, b ibench.Backend, provenance ibench.BackendProvenance) Env {
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
			doc := Doc{SchemaVersion: ibench.CurrentSchemaVersion, Env: benchEnv(t, b)}
			ensureRedis := func(t *testing.T) { b.EnsureImage(t, redisImage) }
			ensureNginx := func(t *testing.T) { b.EnsureImage(t, nginxImage) }

			// Cold: remove the image so the run includes the pull.
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/cold",
				func(t *testing.T) { b.EnsureImageAbsent(t, redisImage) },
				nil,
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, redisImage, "6379/tcp")
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/warm", nil, ensureRedis,
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, redisImage, "6379/tcp")
					return terminateCleanup(t, ctr), err
				})
			b.EnsureImage(t, nginxImage)
			runScenario(t, &doc, b, LibraryContainerGo, nginxImage, "run/warm-nginx", nil, ensureNginx,
				func(t *testing.T) (func(), error) {
					ctr, err := containerGoStart(t, nginxImage, "80/tcp")
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/no-wait", nil, ensureRedis,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage)
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/forlog", nil, ensureRedis,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage,
						container.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
					return terminateCleanup(t, ctr), err
				})
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/forexec", nil, ensureRedis,
				func(t *testing.T) (func(), error) {
					ctr, err := container.Run(context.Background(), redisImage,
						container.WithWaitStrategy(wait.ForExec([]string{"redis-cli", "ping"})))
					return terminateCleanup(t, ctr), err
				})

			// Multi: five sequential containers in one process.
			runScenario(t, &doc, b, LibraryContainerGo, redisImage, "run/multi-5", nil, ensureRedis,
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
			parallelPolicy, ok := ibench.ScenarioPolicyForKey(b.Name, LibraryContainerGo, "run/parallel-8")
			if !ok {
				t.Fatal("no benchmark policy for run/parallel-8")
			}
			for i := 1; i <= parallelPolicy.Iterations; i++ {
				ensureRedis(t)
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
				observation := observeWorkloadImage(t, b, redisImage)
				record(&doc, b.Name, LibraryContainerGo, redisImage, "run/parallel-8", i, elapsed, "", observation)
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
	if b.ImageExists == nil || b.ImageDigest == nil || b.TagImage == nil {
		t.Fatal("Docker image inspect/digest/tag operations are required for the Ryuk benchmark")
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
			if err := verifyTestcontainersRyukDigest(b, ibench.TestcontainersRyukTag); err != nil {
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
		if err := verifyTestcontainersRyukDigest(b, ibench.TestcontainersRyukTag); err != nil {
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

// verifyTestcontainersRyuk proves the running reaper is the pinned image.
// The reference it was started from may be the mutable tag the dependency
// requests or the pinned reference, so acceptance is decided by content: the
// container identity, the image ID behind that reference, and the repository
// digest all have to be the pinned ones.
func verifyTestcontainersRyuk(b ibench.Backend, sessionID string) error {
	if b.ContainerInspect == nil || b.ImageID == nil || b.ImageDigest == nil {
		return fmt.Errorf("Docker reaper identity operations are required for the benchmark")
	}
	name := "reaper_" + sessionID
	identity, err := b.ContainerInspect(name)
	if err != nil {
		return fmt.Errorf("inspect testcontainers reaper %q: %w", name, err)
	}
	if identity.ID == "" || identity.ImageID == "" {
		return fmt.Errorf("testcontainers reaper %q has no container or image identity", name)
	}
	if identity.Name != name {
		return fmt.Errorf("testcontainers reaper name = %q, want %q", identity.Name, name)
	}
	if !validRyukImageReference(identity.ImageReference) {
		return fmt.Errorf("testcontainers reaper image = %q, want %q", identity.ImageReference, strings.Join(testcontainersRyukImageReferences, " or "))
	}
	if identity.Labels["org.testcontainers.sessionId"] != sessionID ||
		identity.Labels["org.testcontainers.reaper"] != "true" ||
		identity.Labels["org.testcontainers.ryuk"] != "true" {
		return fmt.Errorf("testcontainers reaper labels do not match the generated session")
	}
	currentImageID, err := b.ImageID(identity.ImageReference)
	if err != nil {
		return fmt.Errorf("inspect current Ryuk image identity: %w", err)
	}
	if currentImageID != identity.ImageID {
		return fmt.Errorf("reaper image ID = %q, current tag image ID = %q", identity.ImageID, currentImageID)
	}
	return verifyTestcontainersRyukDigest(b, identity.ImageReference)
}

func verifyTestcontainersRyukDigest(b ibench.Backend, image string) error {
	if b.ImageDigest == nil {
		return fmt.Errorf("Ryuk image digest operation is required")
	}
	digest, err := b.ImageDigest(image)
	if err != nil {
		return fmt.Errorf("verify Ryuk image: %w", err)
	}
	if digest != ibench.TestcontainersRyukImageDigest {
		return fmt.Errorf("Ryuk image digest = %q, want %q", digest, ibench.TestcontainersRyukImageDigest)
	}
	return nil
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
	t.Setenv("CONTAINERGO_BACKEND", "docker")
	dockerBackend := ibench.DockerBackend()
	dockerProvenance := configureTestcontainersDockerEndpoint(t, dockerBackend)
	sessionID := requireCanonicalTestcontainersConfig(t)
	requireFreshTestcontainersSession(t, dockerBackend, sessionID)
	env := benchEnvWithProvenance(t, dockerBackend, dockerProvenance)
	env.ReaperSessionID = sessionID
	doc := Doc{SchemaVersion: ibench.CurrentSchemaVersion, Env: env}
	cacheState := prepareTestcontainersRyuk(t, dockerBackend)
	// The workload pull is preparation, not part of session-init timing.
	dockerBackend.EnsureImage(t, redisImage)

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
	if err := verifyTestcontainersRyuk(dockerBackend, sessionID); err != nil {
		_ = ctr.Terminate(context.Background())
		t.Fatalf("session-init Ryuk provenance: %v", err)
	}
	observation := observeWorkloadImage(t, dockerBackend, redisImage)
	record(&doc, "docker", LibraryTestcontainersGo, redisImage, "tc/session-init", 1, elapsed, cacheState, observation)
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	runScenario(t, &doc, dockerBackend, LibraryTestcontainersGo, redisImage, "tc/single", nil, func(t *testing.T) { dockerBackend.EnsureImage(t, redisImage) },
		func(t *testing.T) (func(), error) {
			ctr, err := tc.GenericContainer(context.Background(), tcRequest())
			return tcTerminateCleanup(t, ctr), err
		})

	// Multi: five sequential containers in one process; the session
	// initialization was already paid above.
	runScenario(t, &doc, dockerBackend, LibraryTestcontainersGo, redisImage, "tc/multi-5", nil, func(t *testing.T) { dockerBackend.EnsureImage(t, redisImage) },
		func(t *testing.T) (func(), error) {
			return runTestcontainersMulti(t, func() (tc.Container, error) {
				return tc.GenericContainer(context.Background(), tcRequest())
			})
		})

	if err := ibench.ValidateDoc(doc); err != nil {
		t.Fatalf("validate benchmark result: %v", err)
	}
	path := writeDoc(t, "docker-tc", doc)
	t.Log("\n" + Table(Summarize(doc.Results)))
	t.Logf("results written to %s", path)
}
