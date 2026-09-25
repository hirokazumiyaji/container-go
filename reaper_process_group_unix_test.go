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

func TestSameReaperProcessTableRequiresFixedEdges(t *testing.T) {
	first := map[int][]int{100: {101, 102}, 101: {103}}
	second := map[int][]int{100: {102, 101}, 101: {103}}
	if !sameReaperProcessTable(first, second) {
		t.Fatal("process tables with the same parent-child edges should compare equal")
	}
	third := map[int][]int{100: {101, 102}, 102: {103}}
	if sameReaperProcessTable(first, third) {
		t.Fatal("process tables with a reparented child must not compare equal")
	}
}

func TestReaperProcessSubtreeIgnoresUnrelatedEdges(t *testing.T) {
	table := map[int][]int{
		100: {101},
		101: {102},
		200: {103},
	}
	got := reaperProcessSubtree(table, 100)
	want := map[int][]int{100: {101}, 101: {102}}
	if !sameReaperProcessTable(got, want) {
		t.Fatalf("subtree = %v, want %v", got, want)
	}
}

func TestReaperProcessStartTimeIdentifiesCurrentProcess(t *testing.T) {
	start, err := reaperProcessStartTime(context.Background(), os.Getpid())
	if err != nil {
		t.Fatalf("current process start time: %v", err)
	}
	if start == "" {
		t.Fatal("current process start time is empty")
	}
}

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
	if got, want := fmt.Sprint(signaled), "[101 102 103]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
	eventText := fmt.Sprint(events)
	firstSignal := strings.Index(eventText, "signal:")
	lastLookup := strings.LastIndex(eventText, "lookup:")
	if firstSignal >= 0 && firstSignal < lastLookup {
		t.Fatalf("signaling started before the descendant snapshot completed: %s", eventText)
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

func TestReaperIdentityLookupHonorsAggregateContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := reaperDescendantsWithRefs(
		ctx,
		100,
		func(context.Context, int) ([]int, error) { return nil, nil },
		func(ctx context.Context, _ int) (reaperProcessRef, error) {
			<-ctx.Done()
			return reaperProcessRef{}, ctx.Err()
		},
		func(context.Context, reaperProcessRef) error { return nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("identity lookup error = %v, want aggregate context deadline", err)
	}
}

func TestKillReaperProcessRefNeverFallsBackToNumber(t *testing.T) {
	err := killReaperProcessRefContext(context.Background(), reaperProcessRef{pid: 1 << 30})
	if !errors.Is(err, errReaperProcessIdentityUnavailable) {
		t.Fatalf("unidentified process ref error = %v, want identity-unavailable", err)
	}
}

func TestKillReaperProcessTreatsExitedPIDAsGone(t *testing.T) {
	if err := killReaperProcess(1 << 30); err != nil {
		t.Fatalf("killReaperProcess(exited) = %v, want nil", err)
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

func TestReaperDescendantsReportsLookupError(t *testing.T) {
	lookupErr := errors.New("pgrep failed")
	err := reaperDescendantsWithLookup(
		context.Background(),
		100,
		func(int) error { return nil },
		func(context.Context, int) ([]int, error) { return nil, lookupErr },
	)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want lookup failure", err)
	}
}

func TestReaperDescendantsRetriesFailedBranch(t *testing.T) {
	rootCalls := 0
	childCalls := 0
	lookup := func(_ context.Context, parent int) ([]int, error) {
		switch parent {
		case 100:
			rootCalls++
			if rootCalls == 1 {
				return []int{101}, nil
			}
			return nil, nil
		case 101:
			childCalls++
			if childCalls == 1 {
				return nil, errors.New("temporary pgrep failure")
			}
			return nil, nil
		default:
			return nil, nil
		}
	}
	var signaled []int
	err := reaperDescendantsWithLookup(context.Background(), 100, func(pid int) error {
		signaled = append(signaled, pid)
		return nil
	}, lookup)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookup: %v", err)
	}
	if got, want := fmt.Sprint(signaled), "[101]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
}

func TestReaperDescendantsFindsLateChildOnExpandedBranch(t *testing.T) {
	rootCalls := 0
	childCalls := 0
	lookup := func(_ context.Context, parent int) ([]int, error) {
		switch parent {
		case 100:
			rootCalls++
			if rootCalls == 1 {
				return []int{101}, nil
			}
			return nil, nil
		case 101:
			childCalls++
			if childCalls == 1 {
				return nil, nil
			}
			return []int{102}, nil
		default:
			return nil, nil
		}
	}
	var signaled []int
	err := reaperDescendantsWithLookup(context.Background(), 100, func(pid int) error {
		signaled = append(signaled, pid)
		return nil
	}, lookup)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookup: %v", err)
	}
	if got, want := fmt.Sprint(signaled), "[101 102]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
}

func TestReaperDescendantsDoesNotReexpandReusedPID(t *testing.T) {
	rootCalls := 0
	childCalls := 0
	identityCalls := 0
	lookup := func(_ context.Context, parent int) ([]int, error) {
		switch parent {
		case 100:
			rootCalls++
			if rootCalls == 1 {
				return []int{101}, nil
			}
			return nil, nil
		case 101:
			childCalls++
			return nil, nil
		default:
			return nil, nil
		}
	}
	var signaled []int
	identify := func(pid int) (int, bool) {
		if pid != 101 {
			return 0, false
		}
		identityCalls++
		if len(signaled) == 0 {
			return 101, true
		}
		return 999, true
	}
	err := reaperDescendantsWithLookupAndIdentity(
		context.Background(),
		100,
		func(pid int) error {
			signaled = append(signaled, pid)
			return nil
		},
		lookup,
		identify,
	)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookupAndIdentity: %v", err)
	}
	if got, want := fmt.Sprint(signaled), "[101]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
}

func TestReaperDescendantsRetriesFailedSignal(t *testing.T) {
	signalErr := errors.New("temporary signal failure")
	calls := 0
	err := reaperDescendantsWithLookup(
		context.Background(),
		100,
		func(int) error {
			calls++
			if calls == 1 {
				return signalErr
			}
			return nil
		},
		func(context.Context, int) ([]int, error) { return []int{101}, nil },
	)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookup: %v", err)
	}
	if calls != 2 {
		t.Fatalf("signal calls = %d, want retry", calls)
	}
}

func TestReaperDescendantsHandlesCycles(t *testing.T) {
	lookup := func(_ context.Context, parent int) ([]int, error) {
		if parent == 100 {
			return []int{101}, nil
		}
		return []int{100}, nil
	}
	var signaled []int
	err := reaperDescendantsWithLookup(context.Background(), 100, func(pid int) error {
		signaled = append(signaled, pid)
		return nil
	}, lookup)
	if err != nil {
		t.Fatalf("reaperDescendantsWithLookup: %v", err)
	}
	if got, want := fmt.Sprint(signaled), "[101]"; got != want {
		t.Fatalf("signaled PIDs = %s, want %s", got, want)
	}
}

func TestTrustedReaperPgrepIgnoresPATH(t *testing.T) {
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "pgrep")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)
	path, err := trustedReaperPgrepPath()
	if err != nil {
		t.Skipf("system pgrep unavailable: %v", err)
	}
	if path == fake || !filepath.IsAbs(path) {
		t.Fatalf("trusted pgrep path = %q, want a pinned absolute system path", path)
	}
}

func TestReaperPgrepDistinguishesNoMatchFromFailure(t *testing.T) {
	dir := t.TempDir()
	pgrep := filepath.Join(dir, "pgrep")
	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	children, err := reaperPgrepChildrenWithPath(context.Background(), 100, pgrep)
	if err != nil {
		t.Fatalf("no-match lookup: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("no-match lookup = %v, want empty", children)
	}

	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\necho 'permission denied' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = reaperPgrepChildrenWithPath(context.Background(), 100, pgrep)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("permission error = %v, want propagated pgrep failure", err)
	}

	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = reaperPgrepChildrenWithPath(context.Background(), 100, pgrep)
	if err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("fatal error = %v, want propagated pgrep failure", err)
	}
}

func TestReaperPgrepRejectsExcessiveOutput(t *testing.T) {
	dir := t.TempDir()
	pgrep := filepath.Join(dir, "pgrep")
	if err := os.WriteFile(pgrep, []byte("#!/bin/sh\nawk 'BEGIN { for (i = 0; i < 70000; i++) print 12345 }'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reaperPgrepChildrenWithPath(context.Background(), 100, pgrep); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("excessive pgrep output error = %v", err)
	}
}

func TestReaperPgrepReportsMissingExecutable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-pgrep")
	_, err := reaperPgrepChildrenWithPath(context.Background(), 100, missing)
	if err == nil || !strings.Contains(err.Error(), "pgrep") {
		t.Fatalf("missing pgrep error = %v", err)
	}
}
