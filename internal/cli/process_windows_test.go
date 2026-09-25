//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsProcessTreeCloseRacesTerminate(t *testing.T) {
	for i := 0; i < 100; i++ {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tree := &windowsProcessTree{job: job}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			results <- tree.terminate(nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			tree.close()
			results <- nil
		}()
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("iteration %d: terminate result = %v", i, err)
			}
		}
		if err := tree.terminate(nil); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("iteration %d: terminate after close = %v, want os.ErrProcessDone", i, err)
		}
	}
}

func TestWindowsProcessTreeReleasedHandleDoesNotUseProcess(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/D", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cmd.exe is unavailable: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := &windowsProcessTree{job: job}
	tree.close()
	if err := tree.terminate(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate released tree = %v, want os.ErrProcessDone", err)
	}
}
