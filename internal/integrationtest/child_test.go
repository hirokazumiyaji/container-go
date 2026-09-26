package integrationtest

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const shortChildTimeout = 50 * time.Millisecond

// A silent reader must not block past the declared bound. The previous
// implementation checked the deadline only before the blocking Read, so a
// child that never wrote held the test until the package-wide timeout.
func TestReadReadyFailsOnSilentChild(t *testing.T) {
	// A pipe held open and never written to, so Read blocks indefinitely.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	start := time.Now()
	out, err := ReadReady(pr, shortChildTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("ReadReady succeeded for a silent child: %q", out)
	}
	if !strings.Contains(err.Error(), "readiness") {
		t.Errorf("err=%v", err)
	}
	// The bound must actually be enforced rather than merely requested.
	if elapsed > 20*shortChildTimeout {
		t.Errorf("ReadReady blocked for %v, far beyond its %v bound", elapsed, shortChildTimeout)
	}
}

// A ready marker must be returned promptly.
func TestReadReadyReturnsOnMarker(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("noise\nREADY: myctr running\n"))
		_ = pw.Close()
	}()

	out, err := ReadReady(pr, 30*time.Second)
	if err != nil {
		t.Fatalf("ReadReady: %v", err)
	}
	if out != "noise\nREADY: myctr running\n" {
		t.Errorf("out=%q", out)
	}
}

// A child error marker must also end the read.
func TestReadReadyReturnsOnChildError(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("CHILD-ERROR: no such container\n"))
		_ = pw.Close()
	}()

	out, err := ReadReady(pr, 30*time.Second)
	if err != nil {
		t.Fatalf("ReadReady: %v", err)
	}
	if out != "CHILD-ERROR: no such container\n" {
		t.Errorf("out=%q", out)
	}
}

// Two nonces from separate calls must differ, so concurrent runs cannot
// collide on a resource name.
func TestNonceIsUniquePerCall(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		n := Nonce()
		if n == "" {
			t.Fatal("empty nonce")
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
	}
}

// A nonce must be usable in a container name and a reuse group.
func TestNonceIsNameSafe(t *testing.T) {
	n := Nonce()
	for _, r := range n {
		isDigit := r >= '0' && r <= '9'
		isLowerHex := r >= 'a' && r <= 'f'
		if !isDigit && !isLowerHex {
			t.Fatalf("nonce %q contains %q, which is not name-safe", n, r)
		}
	}
}

// Killing a group twice must be safe, because the cleanup path runs on both
// a normal return and a failure.
func TestChildGroupKillIsIdempotent(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	g := &ChildGroup{}
	g.Track(cmd)
	g.Kill()
	g.Kill()
}

// A child tracked but never started must not panic on cleanup.
func TestChildGroupKillToleratesUnstartedChild(t *testing.T) {
	g := &ChildGroup{}
	g.Track(exec.Command("definitely-not-a-real-binary-xyz"))
	g.Kill()
}
