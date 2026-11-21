package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWithPostReadyHookRunsInOrderAfterRun(t *testing.T) {
	f := newTestRunner()
	var order []string
	var sawContainer *Container
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithPostReadyHook(func(_ context.Context, c *Container) error {
			order = append(order, "first")
			sawContainer = c
			return nil
		}),
		WithPostReadyHook(func(_ context.Context, c *Container) error {
			order = append(order, "second")
			if c != sawContainer {
				t.Error("hooks received different container handles")
			}
			if !c.Created() {
				t.Error("hook ran for a handle that did not report Created")
			}
			if _, err := c.Inspect(context.Background()); err != nil {
				t.Errorf("hook could not inspect the ready container: %v", err)
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil || ctr != sawContainer {
		t.Fatalf("Run handle = %v, hook handle = %v", ctr, sawContainer)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("hook order = %v", order)
	}
}

func TestWithPostReadyHookRejectsNil(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithPostReadyHook(nil))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if !strings.Contains(err.Error(), "hook must not be nil") {
		t.Fatalf("error = %v, want nil-hook validation", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("backend CLI calls = %d, want 0", got)
	}
}

func TestWithPostReadyHookFailureRollsBackOrdinaryRun(t *testing.T) {
	f := newTestRunner()
	sentinel := errors.New("initialization failed")
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithPostReadyHook(func(context.Context, *Container) error { return sentinel }))
	if ctr != nil {
		t.Fatalf("Run returned handle %v after hook failure", ctr)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want hook error", err)
	}
	if !strings.Contains(err.Error(), "post-ready hook 1") {
		t.Fatalf("error = %v, want failing hook index", err)
	}
	if f.callWith("delete") == nil {
		t.Error("hook failure did not roll back the ordinary container")
	}
}

func TestWithPostReadyHookFailureLeavesSharedReuseContainer(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	sentinel := errors.New("initialization failed")
	var sawReused bool
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}),
		WithPostReadyHook(func(_ context.Context, c *Container) error {
			sawReused = c.Reused()
			return sentinel
		}))
	if ctr != nil {
		t.Fatalf("Run returned handle %v after hook failure", ctr)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want hook error", err)
	}
	if !sawReused {
		t.Error("attach hook did not report a reused handle")
	}
	if f.callWith("delete") != nil {
		t.Error("hook failure deleted a shared reuse container")
	}
}

func TestWithPostReadyHookFailureDiscardsCreatedReuseGeneration(t *testing.T) {
	f := newReuseCreateRunner()
	sentinel := errors.New("initialization failed")
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}),
		WithPostReadyHook(func(context.Context, *Container) error { return sentinel }))
	if ctr != nil {
		t.Fatalf("Run returned handle %v after creating caller hook failure", ctr)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want hook error", err)
	}
	if f.callWith("delete") == nil {
		t.Error("creating caller hook failure did not discard the reuse generation")
	}
}
