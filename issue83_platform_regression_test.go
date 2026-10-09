package container

import (
	"context"
	"strings"
	"testing"
)

type appleVariantsDefaultRunner struct {
	*fakeRunner
}

func (r *appleVariantsDefaultRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"arm64"}}]}]`), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestEmptyPlatformSelectorIsUnconstrainedBeforeVariantMatching(t *testing.T) {
	if !platformSelectorMatches("", "malformed/platform/metadata/extra") {
		t.Fatal("empty platform selector was constrained by malformed actual metadata")
	}
	if !(appleEngine{}).platformCompatible("", "malformed/platform/metadata/extra") {
		t.Fatal("Apple empty platform selector was not unconstrained")
	}
}

func TestAppleDefaultRunAcceptsVariantsWithoutPlatformOption(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &appleVariantsDefaultRunner{fakeRunner: base}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("default-platform"), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil {
		t.Fatal("Run returned a nil container")
	}
	sawImageInspect := false
	for _, args := range base.calls {
		if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
			sawImageInspect = true
			if strings.Contains(strings.Join(args, " "), "--platform") {
				t.Fatalf("default Apple image inspect unexpectedly selected a platform: %v", args)
			}
		}
	}
	if !sawImageInspect {
		t.Fatal("default Run did not inspect the Apple image")
	}
}
