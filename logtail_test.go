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
	ctr := &Container{id: "myctr", runner: tr, eng: appleEngine{}}
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

func TestTailWriterKeepsTrailingBytes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{name: "trims_prefix", input: "abcdef", n: 4, want: "cdef"},
		{name: "shorter_than_limit", input: "ab", n: 4, want: "ab"},
		{name: "large_input", input: strings.Repeat("x", 100000) + "MARK", n: 10, want: "xxxxxxMARK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tw := newTailWriter(tc.n)
			if _, err := tw.Write([]byte(tc.input)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := tw.String(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
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
