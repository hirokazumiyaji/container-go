package container

import (
	"context"
	"testing"
)

func TestCreatedReportsOrdinaryRun(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if !ctr.Created() {
		t.Error("Created() = false for a plain Run")
	}
	if ctr.Reused() {
		t.Error("Reused() = true for a plain Run")
	}
}

func TestCreatedDistinguishesReuseCreateFromAttach(t *testing.T) {
	f := newReuseCreateRunner()
	created, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !created.Created() || created.Reused() {
		t.Fatalf("creating caller Created() = %v, Reused() = %v", created.Created(), created.Reused())
	}

	attached, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if attached.Created() || !attached.Reused() {
		t.Fatalf("attaching caller Created() = %v, Reused() = %v", attached.Created(), attached.Reused())
	}
}

func TestSharedPreservesCreatedState(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	shared := ctr.Shared()
	if shared == nil {
		t.Fatal("Shared() = nil")
	}
	if !shared.Created() || shared.Reused() {
		t.Fatalf("ordinary Shared handle Created() = %v, Reused() = %v", shared.Created(), shared.Reused())
	}
}

func TestSharedPreservesReusedState(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	shared := ctr.Shared()
	if shared == nil {
		t.Fatal("Shared() = nil")
	}
	if shared.Created() || !shared.Reused() {
		t.Fatalf("reused Shared handle Created() = %v, Reused() = %v", shared.Created(), shared.Reused())
	}
}
