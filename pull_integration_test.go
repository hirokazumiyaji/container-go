//go:build integration

package container

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/bench"
	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// recordedArgs returns a copy of the arg vectors of every counted call.
func (r *countingRunner) recordedArgs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.args...)
}

// isPullCall reports whether one CLI invocation is an image fetch.
func isPullCall(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch {
	case args[0] == "pull": // docker
		return true
	case args[0] == "image" && len(args) > 1 && args[1] == "pull": // apple
		return true
	default:
		return false
	}
}

// TestIntegrationPullSingleflight removes the image and fires ten
// parallel Runs of it: every Run must succeed and the fetch must happen
// exactly once across all runners.
func TestIntegrationPullSingleflight(t *testing.T) {
	// This benchmark owns teardown; do not inherit diagnostic retention
	// from the caller's environment.
	t.Setenv("CONTAINERGO_KEEP", "0")
	const n = 10
	for _, b := range []bench.Backend{bench.DockerBackend(), bench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			eng, ok := benchEngines(b)
			if !ok {
				t.Fatalf("no engine for backend %s", b.Name)
			}
			image := bench.PinnedRedisImage
			b.EnsureImageAbsent(t, image)

			runners := make([]*countingRunner, n)
			errs := make([]error, n)
			containers := make([]*Container, n)
			var wg sync.WaitGroup
			for i := range n {
				runners[i] = newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctr, err := Run(context.Background(), image,
						withRunner(runners[i]), withEngine(eng))
					containers[i] = ctr
					if err != nil {
						errs[i] = err
					}
				}()
			}
			wg.Wait()
			for i, ctr := range containers {
				if ctr == nil {
					continue
				}
				if err := ctr.Terminate(context.Background()); err != nil {
					t.Logf("terminate run %d: %v", i, err)
				}
			}

			pulls := 0
			for _, r := range runners {
				for _, args := range r.recordedArgs() {
					if isPullCall(args) {
						pulls++
					}
				}
			}
			if pulls != 1 {
				t.Errorf("pulls = %d, want 1", pulls)
			}
			for i, err := range errs {
				if err != nil {
					t.Errorf("run %d: %v", i, err)
				}
			}

			// A warm Run must not pull again.
			r := newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
			ctr, err := Run(context.Background(), image, withRunner(r), withEngine(eng))
			if err != nil {
				if ctr != nil {
					if termErr := ctr.Terminate(context.Background()); termErr != nil {
						t.Logf("terminate warm run: %v", termErr)
					}
				}
				t.Fatalf("warm run: %v", err)
			}
			if termErr := ctr.Terminate(context.Background()); termErr != nil {
				t.Logf("terminate warm run: %v", termErr)
			}
			if slices.ContainsFunc(r.recordedArgs(), isPullCall) {
				t.Error("warm run pulled again")
			}
		})
	}
}
