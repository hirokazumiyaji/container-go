package container

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestDockerOrdinaryPruneUsesStoppedDeleteArgs(t *testing.T) {
	uid := strings.Repeat("5", 64)
	r := &dockerPruneRunner{uid: uid, state: "exited"}
	if _, err := pruneWith(context.Background(), r, dockerEngine{}); err != nil {
		t.Fatalf("pruneWith: %v", err)
	}
	if len(r.deleteArgs) != 1 {
		t.Fatalf("rm calls = %v, want one", r.deleteArgs)
	}
	if slices.Contains(r.deleteArgs[0], "--force") {
		t.Errorf("rm = %v, want no --force for ordinary stopped Prune", r.deleteArgs[0])
	}
}

func TestDockerReuseGroupPruneRetainsForceDeleteArgs(t *testing.T) {
	uid := strings.Repeat("6", 64)
	r := &dockerPruneRunner{uid: uid, state: "running", reuse: true, reuseGroup: "integration"}
	if _, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, "integration"); err != nil {
		t.Fatalf("pruneReuseGroupWith: %v", err)
	}
	if len(r.deleteArgs) != 1 {
		t.Fatalf("rm calls = %v, want one", r.deleteArgs)
	}
	if !slices.Contains(r.deleteArgs[0], "--force") {
		t.Errorf("rm = %v, want --force for explicit reuse-group teardown", r.deleteArgs[0])
	}
}

func TestAppleOrdinaryPruneUsesStoppedDeleteArgs(t *testing.T) {
	f := &lsRunner{
		fakeRunner:  newTestRunner(),
		lsJSON:      pruneLsJSON,
		inspectJSON: stoppedPruneInspect,
	}
	if _, err := pruneWith(context.Background(), f, appleEngine{}); err != nil {
		t.Fatalf("pruneWith: %v", err)
	}
	call := f.callWith("delete")
	if call == nil {
		t.Fatal("stopped managed container was not deleted")
	}
	if slices.Contains(call, "--force") {
		t.Errorf("delete = %v, want no --force for ordinary Apple Prune", call)
	}
}
