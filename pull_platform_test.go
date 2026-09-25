package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureImagePassesPlatformToPull(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	cfg := &config{runner: f, eng: dockerEngine{}, pullPolicy: PullAlways, platform: "linux/amd64", name: "myctr"}
	if err := cfg.ensureImage(context.Background(), "redis:7-alpine"); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	var pullArgs []string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "pull" {
			pullArgs = c
		}
	}
	if pullArgs == nil {
		t.Fatal("no pull call")
	}
	joined := strings.Join(pullArgs, " ")
	if !strings.Contains(joined, "--platform linux/amd64") {
		t.Errorf("pull args missing platform: %v", pullArgs)
	}
}

func TestEnsureImagePassesPlatformToInspect(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	cfg := &config{runner: f, eng: dockerEngine{}, pullPolicy: PullNever, platform: "linux/amd64", name: "myctr"}
	if _, err := imageExists(context.Background(), f, dockerEngine{}, "redis:7-alpine", "linux/amd64"); err != nil {
		t.Fatalf("imageExists: %v", err)
	}
	found := false
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "image" && c[1] == "inspect" {
			if strings.Contains(strings.Join(c, " "), "--platform linux/amd64") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("inspect missing platform: %v", f.calls)
	}
	_ = cfg
}

func TestPullNeverWithPlatformReportsMissing(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = false
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/amd64"),
		withRunner(f), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("error = %v, want ErrImageNotFound", err)
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("error %v does not mention platform", err)
	}
	if f.callWith("run") != nil {
		t.Error("run was called despite missing image")
	}
}

func TestPlatformEmptyPreservesCallCounts(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = ctr
	// image inspect + run, no platform flags.
	for _, c := range f.calls {
		for _, a := range c {
			if a == "--platform" {
				t.Fatalf("unexpected --platform in %v", c)
			}
		}
	}
}

func TestAppleParseImageExistsPlatform(t *testing.T) {
	data := []byte(`[{"descriptor":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"variants":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","platform":{"os":"linux","architecture":"arm64"}}]}]`)
	if !(appleEngine{}).parseImageExists(data, "linux/arm64") {
		t.Error("want match for linux/arm64")
	}
	if (appleEngine{}).parseImageExists(data, "linux/amd64") {
		t.Error("want mismatch for linux/amd64")
	}
	if !(appleEngine{}).parseImageExists(data, "") {
		t.Error("empty platform must mean present")
	}
}

func TestAppleParseImageExistsVariantMismatch(t *testing.T) {
	// Variant-less image must not satisfy a variant-pinned request.
	data := []byte(`[{"descriptor":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"variants":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","platform":{"os":"linux","architecture":"arm"}}]}]`)
	if (appleEngine{}).parseImageExists(data, "linux/arm/v7") {
		t.Error("variant-less arm must not match linux/arm/v7")
	}
	dataV7 := []byte(`[{"descriptor":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"variants":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","platform":{"os":"linux","architecture":"arm","variant":"v7"}}]}]`)
	if !(appleEngine{}).parseImageExists(dataV7, "linux/arm/v7") {
		t.Error("v7 must match linux/arm/v7")
	}
}
