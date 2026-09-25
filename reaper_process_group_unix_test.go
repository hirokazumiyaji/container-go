//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReaperDescendantsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- reaperDescendantsWithLookup(ctx, 1, nil, func(ctx context.Context, _ int) ([]int, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded descendant lookup did not return")
	}
}

func TestReaperDescendantsWalksNestedTree(t *testing.T) {
	graph := map[int][]int{
		100: {101},
		101: {102},
		102: nil,
	}
	var visited []int
	err := reaperDescendantsWithLookup(
		context.Background(),
		100,
		func(pid int) error { visited = append(visited, pid); return nil },
		func(_ context.Context, parent int) ([]int, error) { return graph[parent], nil },
	)
	if err != nil {
		t.Fatalf("nested traversal: %v", err)
	}
	if len(visited) != 2 || visited[0] != 101 || visited[1] != 102 {
		t.Fatalf("visited = %v, want [101 102]", visited)
	}
}

func TestReaperDescendantsStopsAtTraversalLimit(t *testing.T) {
	var visits int
	err := reaperDescendantsWithLookupAndIdentity(
		context.Background(),
		1,
		func(int) error { visits++; return nil },
		func(_ context.Context, parent int) ([]int, error) { return []int{parent + 1}, nil },
		nil,
	)
	if !errors.Is(err, errReaperDescendantLimit) {
		t.Fatalf("error = %v, want descendant traversal limit", err)
	}
	if visits == 0 || visits > maxReaperDescendantPIDs {
		t.Fatalf("visits = %d, want a bounded nonzero traversal", visits)
	}
}
