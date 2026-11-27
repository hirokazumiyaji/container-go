package container

import (
	"context"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

func procNetIPv6Line(port int, state string) string {
	return "   0: 00000000000000000000000000000000:" + strings.ToUpper(procNetHexPort(port)) +
		" 00000000000000000000000000000000:0000 " + state +
		" 00000000:00000000 00:00000000 00000000 999 0 12345 1 0000000000000000 100 0 0 10 0\n"
}

func procNetHexPort(port int) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 4)
	for i := 3; i >= 0; i-- {
		out[i] = digits[port&0xF]
		port >>= 4
	}
	return string(out)
}

// procNetLargeTable builds a table whose IPv4 section alone exceeds the
// former 1MiB bounded capture, optionally followed by the IPv6 table.
func procNetLargeTable(ipv4Lines int, ipv6Tail bool) string {
	var table strings.Builder
	table.WriteString(procNetHeader)
	for i := 0; i < ipv4Lines; i++ {
		table.WriteString(procNetLine(20000+i%40000, "01"))
	}
	if ipv6Tail {
		table.WriteString(procNetHeader)
		table.WriteString(procNetIPv6Line(6379, "0A"))
	}
	return table.String()
}

func TestWaitTargetContainerPortStatusScansPastExecCaptureLimit(t *testing.T) {
	// The combined /proc/net/tcp output used to be truncated at 1MiB, so a
	// busy container's IPv6 table never reached the parser and an IPv6-only
	// listener timed out. The streaming scan must see the tail.
	table := procNetLargeTable(9000, true)
	if ipv4 := strings.Index(table, procNetIPv6Line(6379, "0A")); ipv4 <= 1<<20 {
		t.Fatalf("IPv6 listener offset = %d, want beyond the former 1MiB capture limit", ipv4)
	}
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: table}
	target := newDockerWaitTarget(f)

	status, err := target.ContainerPortStatus(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("ContainerPortStatus: %v", err)
	}
	if status != wait.PortListenOpen {
		t.Fatalf("status = %v, want open for an IPv6 listener past 1MiB", status)
	}
}

// resetProcNetScanner clears parse state so a reused scanner can measure
// only Write; the scanner allocation stays outside the measured region.
func resetProcNetScanner(scanner *procNetScanner) {
	scanner.line = scanner.line[:0]
	scanner.listening = false
	scanner.loopbackListening = false
	scanner.supported = false
	scanner.skipLine = false
}

func TestProcNetScannerDoesNotAllocate(t *testing.T) {
	lines := strings.Repeat(procNetLine(8081, "01"), 64)
	tests := map[string]struct {
		data       []byte
		wantListen bool
	}{
		"match on the first socket line": {
			data:       []byte(procNetHeader + procNetLine(6379, "0A") + lines),
			wantListen: true,
		},
		"full scan without a match": {
			data:       []byte(procNetHeader + lines),
			wantListen: false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			scanner := newProcNetScanner(6379)
			allocs := testing.AllocsPerRun(100, func() {
				resetProcNetScanner(scanner)
				_, _ = scanner.Write(tc.data)
			})
			if !scanner.supported {
				t.Fatal("supported = false, want a parsed /proc table")
			}
			if scanner.listening != tc.wantListen {
				t.Fatalf("listening = %v, want %v", scanner.listening, tc.wantListen)
			}
			if allocs != 0 {
				t.Fatalf("allocations = %v per scan, want zero", allocs)
			}
		})
	}
}

func TestProcNetScannerStopsParsingAfterMatch(t *testing.T) {
	scanner := newProcNetScanner(6379)
	if _, err := scanner.Write([]byte(procNetHeader + procNetLine(6379, "0A"))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := scanner.Write([]byte("unterminated-fragment")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(scanner.line) != 0 {
		t.Fatalf("retained row buffer = %q after a match, want parsing stopped", scanner.line)
	}
	if !scanner.listening || !scanner.supported {
		t.Fatalf("listening = %v, supported = %v, want both true", scanner.listening, scanner.supported)
	}
}

func BenchmarkProcNetScannerEarlyMatch(b *testing.B) {
	data := []byte(procNetHeader + procNetLine(6379, "0A") + procNetLargeTable(9000, false))
	scanner := newProcNetScanner(6379)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetProcNetScanner(scanner)
		_, _ = scanner.Write(data)
		if !scanner.listening || !scanner.supported {
			b.Fatal("first-line listener was not found")
		}
	}
}

func BenchmarkProcNetScannerFullScan(b *testing.B) {
	data := []byte(procNetLargeTable(9000, false))
	scanner := newProcNetScanner(6379)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetProcNetScanner(scanner)
		_, _ = scanner.Write(data)
		if scanner.listening || !scanner.supported {
			b.Fatal("unexpected scan result")
		}
	}
}

func TestProcNetScannerHandlesChunkBoundaries(t *testing.T) {
	// The overlong row spans Write calls at every chunk size, exercising
	// the skip-until-newline branch, and the row after it must still match.
	table := procNetHeader + strings.Repeat("x", procNetMaxLineBytes+1) + "\n" + procNetIPv6Line(6379, "0A")
	for _, chunk := range []int{1, 7, 4096} {
		scanner := newProcNetScanner(6379)
		for start := 0; start < len(table); start += chunk {
			end := min(start+chunk, len(table))
			if _, err := scanner.Write([]byte(table[start:end])); err != nil {
				t.Fatalf("chunk %d: Write: %v", chunk, err)
			}
			if scanner.listening {
				break
			}
		}
		if !scanner.listening || !scanner.supported {
			t.Fatalf("chunk %d: listening = %v, supported = %v, want both true", chunk, scanner.listening, scanner.supported)
		}
	}
}

func TestProcNetScannerSkipsOverlongLine(t *testing.T) {
	// A malformed line must not grow the retained buffer or hide the rows
	// that follow it.
	table := procNetHeader + strings.Repeat("x", procNetMaxLineBytes+1) + "\n" + procNetLine(6379, "0A")
	scanner := newProcNetScanner(6379)
	if _, err := scanner.Write([]byte(table)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !scanner.listening || !scanner.supported {
		t.Fatalf("listening = %v, supported = %v, want both true", scanner.listening, scanner.supported)
	}
	if cap(scanner.line) > 2*procNetMaxLineBytes {
		t.Fatalf("line buffer capacity = %d, want bounded by the line limit", cap(scanner.line))
	}
}
