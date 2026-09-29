package container

import (
	"context"
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
	ctr := &Container{id: "myctr", runner: tr, eng: appleEngine{}, state: &containerState{}}
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
