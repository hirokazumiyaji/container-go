package container

import (
	"context"
	"errors"
	"io"
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
	tail, err := ctr.logTailWithError(context.Background())
	if err != nil {
		t.Fatalf("logTailWithError: %v", err)
	}
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
		runner: runner,
		eng:    dockerEngine{},
		sync:   &lockPack{uid: oldUID},
	}

	if tail, err := ctr.logTailWithError(context.Background()); tail != "" {
		t.Fatalf("logTailWithError returned replacement logs: %q", tail)
	} else if err == nil {
		t.Fatal("logTailWithError = nil error for a removed immutable target")
	}
	nameHandle := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	if tail, err := nameHandle.logTailWithError(context.Background()); tail != "" {
		t.Fatalf("unverified Docker name-target probe = %q, want no logs", tail)
	} else if err == nil {
		t.Fatal("unverified Docker name-target probe = nil error")
	}
	runner.mu.Lock()
	calls := append([][]string(nil), runner.calls...)
	runner.mu.Unlock()
	var logCalls [][]string
	for _, call := range calls {
		if len(call) > 0 && call[0] == "logs" {
			logCalls = append(logCalls, call)
		}
	}
	if len(logCalls) != 1 || logCalls[0][len(logCalls[0])-1] != oldUID {
		t.Fatalf("Docker log targets = %v, want only the old immutable UID %s", logCalls, oldUID)
	}
}

func TestLogTailUsesSnapshotStreamWithoutMaterializingRunOutput(t *testing.T) {
	const secret = "snapshot-stream-secret-117"
	runner := &snapshotTailRunner{data: []byte("header=" + secret + "\n" + strings.Repeat("x", 128*1024))}
	ctr := &Container{id: "snapshot-tail", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}
	got, err := ctr.logTailWithError(context.Background())
	if err != nil {
		t.Fatalf("logTailWithError: %v", err)
	}
	if runner.runCalled {
		t.Fatal("logTail fell back to materializing Runner.Run output")
	}
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("tail = %q, want streamed redaction", got)
	}
	if len(got) > logTailLimit {
		t.Fatalf("tail length = %d, want <= %d", len(got), logTailLimit)
	}
}

func TestLogTailPreservesSnapshotReadError(t *testing.T) {
	wantErr := errors.New("snapshot read failed")
	runner := &snapshotTailRunner{data: []byte("partial"), readErr: wantErr}
	ctr := &Container{id: "snapshot-error", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}
	got, err := ctr.logTailWithError(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got == "" {
		t.Fatal("partial safe tail was discarded")
	}
}

type snapshotTailRunner struct {
	data      []byte
	readErr   error
	runCalled bool
}

func (r *snapshotTailRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	r.runCalled = true
	return nil, nil, errors.New("unexpected Run call")
}

func (r *snapshotTailRunner) StreamSnapshot(context.Context, ...string) (io.ReadCloser, error) {
	return io.NopCloser(&chunkReader{data: r.data, err: r.readErr}), nil
}

type chunkReader struct {
	data []byte
	off  int
	err  error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
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
