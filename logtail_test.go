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

func TestLogTailUsesSnapshotStreamWithoutMaterializingRunOutput(t *testing.T) {
	const secret = "snapshot-stream-secret-117"
	runner := &snapshotTailRunner{data: []byte("header=" + secret + "\n" + strings.Repeat("x", 128*1024))}
	ctr := &Container{id: "snapshot-tail", runner: runner, eng: appleEngine{}}
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
	ctr := &Container{id: "snapshot-error", runner: runner, eng: appleEngine{}}
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
