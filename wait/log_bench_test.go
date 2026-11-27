package wait

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// BenchmarkScanLogStream measures the per-line hot path: framing, replay
// fingerprinting, and pattern counting for a stream of ordinary log lines
// with the match near the end.
func BenchmarkScanLogStream(b *testing.B) {
	var payload bytes.Buffer
	for i := 0; i < 2000; i++ {
		payload.WriteString("level=info msg=starting worker=12 retry=0 detail=ready-not-yet\n")
	}
	payload.WriteString("level=info msg=\"server ready\"\n")
	data := payload.Bytes()
	match := func(line string) int { return strings.Count(line, "server ready") }
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := scanLogStream(context.Background(), io.NopCloser(bytes.NewReader(data)), match, 1, logReplay{})
		if result.err != nil || !result.found {
			b.Fatalf("scan = err:%v found:%v, want the ready line", result.err, result.found)
		}
	}
}
