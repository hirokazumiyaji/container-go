//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReaperDescendantsSignalsBranchesAndRepeats(t *testing.T) {
	const root = 100
	rootCalls := 0
	var events []string
	lookup := func(_ context.Context, parent int) ([]int, error) {
		events = append(events, fmt.Sprintf("lookup:%d", parent))
		switch parent {
		case root:
			rootCalls++
			switch rootCalls {
			case 1:
				return []int{101}, nil
			case 2:
				return []int{102}, nil
			default:
				return []int{101, 102}, nil
			}
		case 101:
			return []int{103}, nil
		case 102, 103:
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected parent %d", parent)
		}
	}

	var signaled []int
	err := reaperDescendantsWithLookup(context.Background(), root, func(pid int) error {
		events = append(events, fmt.Sprintf("signal:%d", pid))
		signaled = append(signaled, pid)
		return nil
	}, lookup)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookup: %v", err)
	}
	if got, want := fmt.Sprint(signaled), "[101 103 102]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(events), "[lookup:100 lookup:101 signal:101 lookup:103 signal:103 lookup:100 lookup:102 signal:102 lookup:100]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestReaperDescendantsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := reaperDescendantsWithLookup(ctx, 100, func(int) error { return nil }, func(ctx context.Context, _ int) ([]int, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded traversal took %s", elapsed)
	}
}

func TestReaperDescendantsReportsLimit(t *testing.T) {
	rootCalls := 0
	lookup := func(_ context.Context, parent int) ([]int, error) {
		if parent == 100 {
			pid := 101 + rootCalls
			rootCalls++
			return []int{pid}, nil
		}
		return nil, nil
	}
	err := reaperDescendantsWithLookup(context.Background(), 100, func(int) error { return nil }, lookup)
	if !errors.Is(err, errReaperDescendantLimit) {
		t.Fatalf("error = %v, want descendant traversal limit", err)
	}
}

func TestReaperDescendantsReportsSignalError(t *testing.T) {
	signalErr := errors.New("signal failed")
	err := reaperDescendantsWithLookup(
		context.Background(),
		100,
		func(int) error { return signalErr },
		func(context.Context, int) ([]int, error) { return []int{101}, nil },
	)
	if !errors.Is(err, signalErr) {
		t.Fatalf("error = %v, want signal failure", err)
	}
}

func TestReaperPgrepDistinguishesNoMatchFromFailure(t *testing.T) {
	dir := t.TempDir()
	pgrep := filepath.Join(dir, "pgrep")
	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	children, err := reaperPgrepChildren(context.Background(), 100)
	if err != nil {
		t.Fatalf("no-match lookup: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("no-match lookup = %v, want empty", children)
	}

	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\necho 'permission denied' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = reaperPgrepChildren(context.Background(), 100)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("permission error = %v, want propagated pgrep failure", err)
	}

	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = reaperPgrepChildren(context.Background(), 100)
	if err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("fatal error = %v, want propagated pgrep failure", err)
	}
}

func TestReaperPgrepReportsMissingExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := reaperPgrepChildren(context.Background(), 100)
	if err == nil || !strings.Contains(err.Error(), "pgrep") {
		t.Fatalf("missing pgrep error = %v", err)
	}
}
