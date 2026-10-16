package container

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestContainerUIDIsRaceFreeAcrossPromotionAndTerminate is the regression
// for the data race on Container.uid. Promotion writes through
// setImmutableID (uidMu), while Terminate and other lifecycle helpers read
// the same field via immutableID. Readers start before the sole promotion
// so -race can observe an unsynchronized write if a helper still reads
// c.uid without uidMu.
func TestContainerUIDIsRaceFreeAcrossPromotionAndTerminate(t *testing.T) {
	ctr := &Container{
		id:     "c1",
		runner: newTestRunner(),
		eng:    dockerEngine{},
	}
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				_ = ctr.immutableID()
				_, _ = ctr.verifiedOperationTarget(ctx)
				_ = ctr.Terminate(ctx)
			}
		}()
	}

	close(start)
	// Promote once while readers are already sampling the empty UID.
	ctr.setImmutableID(dockerFixtureID)
	wg.Wait()

	if got := ctr.immutableID(); got != dockerFixtureID {
		t.Fatalf("immutableID = %q, want %q", got, dockerFixtureID)
	}
}

// TestContainerImmutableIDNeverDowngrades covers the other half of the
// invariant: once promoted, a later empty or different ID must not clear
// or replace the value, or a concurrent promote could race a reader into
// deleting by name instead of by immutable ID.
func TestContainerImmutableIDNeverDowngrades(t *testing.T) {
	ctr := &Container{id: "c1", runner: newTestRunner(), eng: dockerEngine{}}
	ctr.setImmutableID(dockerFixtureID)
	ctr.setImmutableID("")
	ctr.setImmutableID(strings.Repeat("b", 64))
	if got := ctr.immutableID(); got != dockerFixtureID {
		t.Errorf("immutableID = %q, want the first promoted value %q", got, dockerFixtureID)
	}
}
