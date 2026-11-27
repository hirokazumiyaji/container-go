package container

import (
	"sync"
	"testing"
)

// TestContainerImageRaceFreeAcrossIdentityRemember is the regression for the
// unsynchronized c.image read in Container.Image. rememberIdentity promotes
// the image identity under lockPack.mu after an inspect; readers start before
// that promotion so -race can observe the write if Image still reads the field
// directly.
func TestContainerImageRaceFreeAcrossIdentityRemember(t *testing.T) {
	ctr := &Container{id: "myctr", runner: newTestRunner(), eng: appleEngine{}}
	info := &engineInfo{image: "redis:7-alpine", labels: map[string]string{}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				_ = ctr.Image()
			}
		}()
	}

	close(start)
	// rememberIdentity writes c.image while the readers are sampling it.
	if err := ctr.cacheInfo(info); err != nil {
		t.Fatalf("cacheInfo: %v", err)
	}
	wg.Wait()

	if got := ctr.Image(); got != "redis:7-alpine" {
		t.Fatalf("Image = %q, want redis:7-alpine", got)
	}
}

// Shared copies the handle's image identity too, so it must take the same lock
// as an identity promotion.
func TestSharedImageRaceFreeAcrossIdentityRemember(t *testing.T) {
	ctr := &Container{id: "myctr", runner: newTestRunner(), eng: appleEngine{}}
	info := &engineInfo{image: "redis:7-alpine", labels: map[string]string{}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				_ = ctr.Shared()
			}
		}()
	}

	close(start)
	if err := ctr.cacheInfo(info); err != nil {
		t.Fatalf("cacheInfo: %v", err)
	}
	wg.Wait()

	if got := ctr.Shared().Image(); got != "redis:7-alpine" {
		t.Fatalf("Shared().Image = %q, want redis:7-alpine", got)
	}
}
