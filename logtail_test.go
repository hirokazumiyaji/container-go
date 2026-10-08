package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type tailRunner struct {
	*fakeRunner
	logData string
	sawTail bool
}

func (t *tailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "logs" {
		t.mu.Lock()
		t.calls = append(t.calls, args)
		for _, a := range args {
			if a == "--tail" || a == "-n" {
				t.sawTail = true
			}
		}
		t.mu.Unlock()
		return []byte(t.logData), nil, nil
	}
	return t.fakeRunner.Run(ctx, args...)
}

func TestLogTailContainsTrailingMarker(t *testing.T) {
	// 2 MiB of 'A' followed by a marker: the old first-1MiB logic
	// dropped the marker.
	big := strings.Repeat("A", 2*1024*1024) + "LATEST_FATAL_MARKER"
	base := newTestRunner()
	base.imagePresent = true
	tr := &tailRunner{fakeRunner: base, logData: big}
	ctr := &Container{id: "myctr", runner: tr, eng: appleEngine{}, creation: "0123456789abcdef"}
	tail := ctr.logTail(context.Background())
	if !strings.Contains(tail, "LATEST_FATAL_MARKER") {
		t.Fatalf("tail missing marker, len=%d", len(tail))
	}
	if len(tail) > logTailLimit {
		t.Fatalf("tail len=%d, want <= %d", len(tail), logTailLimit)
	}
	if !tr.sawTail {
		t.Error("logTail did not request CLI-bounded tail")
	}
	if call := tr.callWith("logs"); call == nil || call[len(call)-1] != "myctr" {
		t.Errorf("Apple log target = %v, want logical name", call)
	}
}

type replacementTailRunner struct {
	*fakeRunner
	oldUID      string
	replacement string
}

func (r *replacementTailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "logs" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		if args[len(args)-1] == r.oldUID {
			return nil, nil, errors.New("old Docker UID was removed")
		}
		return []byte(r.replacement), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestLogTailDoesNotReadReplacementLogs(t *testing.T) {
	// The name is already reused by a replacement; querying it would
	// return the replacement's logs, while the old UID is gone.
	const oldUID = "1111111111111111111111111111111111111111111111111111111111111111"
	runner := &replacementTailRunner{
		fakeRunner:  newTestRunner(),
		oldUID:      oldUID,
		replacement: "replacement container logs\n",
	}
	ctr := &Container{
		id:     "myctr",
		uid:    oldUID,
		runner: runner,
		eng:    dockerEngine{},
	}

	if tail := ctr.logTail(context.Background()); tail != "" {
		t.Fatalf("logTail returned replacement logs: %q", tail)
	}
	nameHandle := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	if tail := nameHandle.logTail(context.Background()); tail != "" {
		t.Fatalf("unverified Docker name-target probe = %q, want no logs", tail)
	}
	runner.mu.Lock()
	calls := append([][]string(nil), runner.calls...)
	runner.mu.Unlock()
	var logCall []string
	for _, call := range calls {
		if len(call) > 0 && call[0] == "logs" {
			logCall = call
			break
		}
	}
	if logCall == nil || logCall[len(logCall)-1] != oldUID {
		t.Fatalf("Docker log target = %v, want old immutable UID %s", logCall, oldUID)
	}
}

func TestLastNBytesKeepsTail(t *testing.T) {
	got := lastNBytes(strings.NewReader("abcdef"), 4)
	if got != "cdef" {
		t.Fatalf("got %q, want cdef", got)
	}
	got = lastNBytes(strings.NewReader("ab"), 4)
	if got != "ab" {
		t.Fatalf("got %q, want ab", got)
	}
	big := strings.Repeat("x", 100000) + "MARK"
	got = lastNBytes(strings.NewReader(big), 10)
	if got != "xxxxxxMARK" {
		t.Fatalf("got %q", got)
	}
}

func TestLogsTailArgsBounded(t *testing.T) {
	docker := dockerEngine{}.logsTailArgs("myctr")
	joined := strings.Join(docker, " ")
	if !strings.Contains(joined, "--tail") {
		t.Errorf("docker tail args = %v, want --tail", docker)
	}
	apple := appleEngine{}.logsTailArgs("myctr")
	if len(apple) == 0 || apple[0] != "logs" {
		t.Errorf("apple tail args = %v", apple)
	}
}
