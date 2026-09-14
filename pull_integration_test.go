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
	const n = 10
	for _, b := range []bench.Backend{bench.DockerBackend(), bench.AppleBackend()} {
		t.Run(b.Name, func(t *testing.T) {
			b.Available(t)
			eng, ok := benchEngines(b)
			if !ok {
				t.Fatalf("no engine for backend %s", b.Name)
			}
			image := integrationRedis
			if err := b.RemoveImage(image); err != nil {
				t.Fatalf("remove image: %v", err)
			}

			runners := make([]*countingRunner, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				runners[i] = newCountingRunner(&cli.ExecRunner{Binary: b.Bin})
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctr, err := Run(context.Background(), image,
						withRunner(runners[i]), withEngine(eng))
					if err != nil {
						errs[i] = err
						return
					}
					_ = ctr.Terminate(context.Background())
				}()
			}
			wg.Wait()

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
				t.Fatalf("warm run: %v", err)
			}
			_ = ctr.Terminate(context.Background())
			if slices.ContainsFunc(r.recordedArgs(), isPullCall) {
				t.Error("warm run pulled again")
			}
		})
	}
}
