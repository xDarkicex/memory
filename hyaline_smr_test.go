package memory

import (
	"sync"
	"testing"
)

func TestHyalineSMREnterLeave(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	// Enter multiple shards — always succeeds (store, not CAS).
	for i := 0; i < sfl.numShards*2; i++ {
		sfl.HyalineEnter(i % sfl.numShards)
	}

	// Leave all.
	for i := 0; i < sfl.numShards*2; i++ {
		sfl.HyalineLeave(i % sfl.numShards)
	}

	sfl.Deallocate(slot)
}

func TestHyalineSMRRetireReclaim(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate and retire slots.
	var slots [][]byte
	for i := 0; i < 200; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		slots = append(slots, slot)
	}
	if len(slots) < 65 {
		t.Fatalf("expected at least 65 allocations, got %d", len(slots))
	}

	// Retire enough slots to trigger batch flush (threshold=65).
	for _, slot := range slots[:65] {
		if err := sfl.Retire(slot); err != nil {
			t.Fatalf("Retire failed: %v", err)
		}
	}

	// The retired slots should be reclaimed by Hyaline leave. To trigger
	// reclamation, we need Enter→Leave cycles. Allocate triggers this
	// indirectly via batch refill from the global FreeList, where reclaimed
	// slots land.
	sfl.HyalineEnter(0)
	sfl.HyalineLeave(0)

	// Should be able to allocate (reclaimed slots are back in global FreeList).
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after retire+reclaim failed: %v", err)
	}
	if len(slot) != int(cfg.SlotSize) {
		t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot))
	}
	sfl.Deallocate(slot)
}

func TestHyalineSMRProtectedSlotSurvivesReclamation(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Enter a slot before retiring — the retired nodes should stay queued
	// until we leave.
	sfl.HyalineEnter(0)

	// Allocate and retire enough nodes to flush a batch (threshold=65).
	var slots [][]byte
	for i := 0; i < 65; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		slots = append(slots, slot)
	}

	for _, slot := range slots {
		if err := sfl.Retire(slot); err != nil {
			t.Fatalf("Retire failed: %v", err)
		}
	}

	// Slots are retired but not yet reclaimed — slot 0 is still occupied.
	// Leave slot 0 to trigger reclamation.
	sfl.HyalineLeave(0)

	// Now we should be able to allocate (reclaimed slots are back).
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after leave failed: %v", err)
	}
	sfl.Deallocate(slot)
}

func TestHyalineSMRDoubleRetire(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if err := sfl.Retire(slot); err != nil {
		t.Fatal(err)
	}
	if err := sfl.Retire(slot); err == nil {
		t.Fatal("expected double-retire error")
	}
}

func TestHyalineSMRConcurrentEnterLeaveRetire(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	const goroutines = 8
	const opsPerGoroutine = 200

	// Pre-allocate slots so we don't exhaust during the test.
	var slots [][]byte
	for i := 0; i < goroutines*opsPerGoroutine; i++ {
		s, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("pre-allocate failed at %d: %v", i, err)
		}
		slots = append(slots, s)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			shardIdx := base % sfl.numShards
			for i := 0; i < opsPerGoroutine; i++ {
				slot := slots[base+i]

				sfl.HyalineEnter(shardIdx)
				_ = slot[0]
				sfl.HyalineLeave(shardIdx)

				if err := sfl.Retire(slot); err != nil {
					errCh <- err
					return
				}
			}
		}(g * opsPerGoroutine)
	}
	wg.Wait()
	close(errCh)

	for e := range errCh {
		t.Error(e)
	}

	// All slots should be reclaimable.
	for i := 0; i < 100; i++ {
		s, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("re-allocate after concurrent retire failed at %d: %v", i, err)
		}
		sfl.Deallocate(s)
	}
}
