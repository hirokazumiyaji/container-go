package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type malformedDockerInspectRunner struct {
	deletes []string
}

func (r *malformedDockerInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(`[{"Id":"not-a-container-id","Name":"/myctr"}]`), nil, nil
	case "rm", "delete":
		r.deletes = append(r.deletes, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDockerTerminateDoesNotTreatMalformedMatchingInspectAsMissing(t *testing.T) {
	r := &malformedDockerInspectRunner{}
	ctr := &Container{id: "myctr", runner: r, eng: dockerEngine{}}

	err := ctr.Terminate(context.Background())
	if err == nil {
		t.Fatal("Terminate accepted malformed matching inspect output")
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Terminate error = %v, want protocol failure rather than not-found", err)
	}
	if len(r.deletes) != 0 {
		t.Fatalf("delete calls = %v, want no delete after malformed inspect", r.deletes)
	}
}

func TestDockerFailedCreateCleanupDoesNotSkipMalformedMatchingInspect(t *testing.T) {
	r := &malformedDockerInspectRunner{}
	cfg := &config{
		name:     "myctr",
		creation: "aaaaaaaaaaaaaaaa",
		runner:   r,
		eng:      dockerEngine{},
	}

	err := cleanupFailedCreate(context.Background(), cfg, errors.New("run failed"), errors.New("run failed"))
	if err == nil {
		t.Fatal("cleanupFailedCreate accepted malformed matching inspect output")
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("cleanup error = %v, want protocol failure rather than not-found", err)
	}
	if !strings.Contains(err.Error(), "cleanup container myctr: inspect") {
		t.Fatalf("cleanup error = %v, want inspect failure", err)
	}
	if len(r.deletes) != 0 {
		t.Fatalf("delete calls = %v, want no delete after malformed inspect", r.deletes)
	}
}
