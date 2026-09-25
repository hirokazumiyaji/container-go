package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func creationInspectJSON(name, creation string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q,
        "com.github.hirokazumiyaji.container-go.creation": %q
      }
    },
    "status": {"state": "running", "networks": []}
  }
]`, name, name, sessionID(), creation)
}

type genRunner struct {
	*fakeRunner
	inspectJSON string
	deleted     []string
}

func (g *genRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		g.mu.Lock()
		g.calls = append(g.calls, args)
		g.mu.Unlock()
		return []byte(g.inspectJSON), nil, nil
	case "delete", "rm":
		g.mu.Lock()
		g.calls = append(g.calls, args)
		g.deleted = append(g.deleted, args[len(args)-1])
		g.mu.Unlock()
		return nil, nil, nil
	default:
		return g.fakeRunner.Run(ctx, args...)
	}
}

func TestTerminateRefusesReplacedContainer(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	oldCreation := "aaaaaaaaaaaaaaaa"
	newCreation := "bbbbbbbbbbbbbbbb"
	g := &genRunner{fakeRunner: base, inspectJSON: creationInspectJSON("myctr", newCreation)}
	ctr := &Container{
		id: "myctr", runner: g, eng: appleEngine{},
		creation: oldCreation,
	}
	if err := ctr.Terminate(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want ErrGenerationReplaced", err)
	}
	if len(g.deleted) != 0 {
		t.Fatalf("deleted = %v, want no delete", g.deleted)
	}
}

func TestTerminateDeletesSameGeneration(t *testing.T) {
	base := newTestRunner()
	creation := "cccccccccccccccc"
	g := &genRunner{fakeRunner: base, inspectJSON: creationInspectJSON("myctr", creation)}
	ctr := &Container{
		id: "myctr", runner: g, eng: appleEngine{},
		creation: creation,
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(g.deleted) != 1 {
		t.Fatalf("deleted = %v, want [myctr]", g.deleted)
	}
}

func TestRunAddsCreationLabel(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	ctr := runTestContainer(t, f)
	if ctr.creation == "" {
		t.Fatal("creation empty")
	}
	if !creationRE.MatchString(ctr.creation) {
		t.Fatalf("creation = %q, want hex", ctr.creation)
	}
	joined := strings.Join(f.callWith("run"), " ")
	if !strings.Contains(joined, creationLabel+"="+ctr.creation) {
		t.Errorf("run args missing creation label: %s", joined)
	}
}

func TestReaperSkipsReplacedGeneration(t *testing.T) {
	// Stub binary: inspect prints the *current* creation, delete logs.
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/ctr"
	currentCreation := "dddddddddddddddd"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo \"" + currentCreation + "\"; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	oldCreation := "eeeeeeeeeeeeeeee"
	if err := r.register("myctr", oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	// Give the reaper a moment to run; it must NOT delete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 && strings.Contains(string(data), "myctr") {
		t.Fatalf("reaper deleted replaced container: %q", data)
	}
}

func TestReaperDeletesMatchingGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/ctr"
	creation := "ffffffffffffffff"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\",'; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("myctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force myctr")
}
