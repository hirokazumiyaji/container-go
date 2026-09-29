package container

import (
	"context"
	"strings"
	"testing"
)

func TestWithLabelsRejectsReserved(t *testing.T) {
	for _, k := range []string{
		"com.github.hirokazumiyaji.container-go",
		"com.github.hirokazumiyaji.container-go.session",
		"com.github.hirokazumiyaji.container-go.reuse",
		"com.github.hirokazumiyaji.container-go.reuse-group",
		"com.github.hirokazumiyaji.container-go.creation",
	} {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithLabels(map[string]string{k: "x"}),
			withRunner(newTestRunner()), withEngine(appleEngine{}))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("key %q: err = %v, want reserved", k, err)
		}
	}
}

func TestTerminateRefusesWhenLabelMissing(t *testing.T) {
	base := newTestRunner()
	g := &genRunner{fakeRunner: base, inspectJSON: `[
  {"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{"com.github.hirokazumiyaji.container-go":"true"}},"status":{"state":"running","networks":[]}}
]`}
	ctr := &Container{id: "myctr", runner: g, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa", state: &containerState{}}
	if err := ctr.Terminate(context.Background()); err == nil {
		t.Fatal("want error when generation label absent")
	}
	if len(g.deleted) != 0 {
		t.Fatalf("deleted = %v, want none", g.deleted)
	}
}
