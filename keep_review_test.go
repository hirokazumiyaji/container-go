package container

import (
	"context"
	"errors"
	"testing"
)

func TestReviewKeepSuppressesFailedCreateCleanup(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	base.imagePresent = true
	runner := &failRunRunner{
		fakeRunner:  base,
		runErr:      errors.New("create failed"),
		inspectJSON: ownedInspectJSON("keep-failed"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-failed"), withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded, want create failure")
	}
	if len(runner.deleted) != 0 {
		t.Fatalf("KEEP cleanup issued delete: %v", runner.deleted)
	}
}

func TestReviewKeepSuppressesRollback(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	runner := &rollbackErrorRunner{fakeRunner: base}
	ctr := &Container{id: "keep-rollback", runner: runner, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	cause := errors.New("copy failed")
	if err := ctr.rollback(context.Background(), cause); !errors.Is(err, cause) {
		t.Fatalf("rollback error = %v, want original cause", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("KEEP rollback issued backend calls: %v", runner.calls)
	}
}
