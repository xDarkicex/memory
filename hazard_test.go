package memory

import (
	"sync"
	"testing"
)

func TestHazardProtectUnprotect(t *testing.T) {
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

	// We should be able to protect at least 2 times, and eventually fail
	// when all hazard slots across all shards (numShards * K) are full.
	var guards []HazardGuard
	for {
		guard, ok := sfl.Protect(slot)
		if !ok {
			break
		}
		guards = append(guards, guard)
	}

	if len(guards) < 2 {
		t.Fatalf("expected at least 2 successful Protects, got %d", len(guards))
	}

	// Unprotect all
	for _, g := range guards {
		sfl.Unprotect(g)
	}

	// After unprotect, should be able to protect again.
	guard3, ok := sfl.Protect(slot)
	if !ok {
		t.Fatal("expected Protect after Unprotect to succeed")
	}
	sfl.Unprotect(guard3)

	sfl.Deallocate(slot)
}

func TestHazardRetireAndReclaim(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024 // Small pool to force exhaustion
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate several slots and retire them (not Deallocate).
	var slots [][]byte
	for i := 0; i < 64; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		slots = append(slots, slot)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one allocation")
	}

	// Retire all slots (goes to retirement list, not recycled cache).
	for _, slot := range slots {
		if err := sfl.Retire(slot); err != nil {
			t.Fatalf("Retire failed: %v", err)
		}
	}

	// Now allocate again — should trigger scan and reclaim retired slots.
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after retire+scan failed: %v", err)
	}
	if len(slot) != int(cfg.SlotSize) {
		t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot))
	}
	sfl.Deallocate(slot)
}

func TestHazardProtectedSlotSurvivesScan(t *testing.T) {
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

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	// Protect the slot — it should survive the scan.
	guard, ok := sfl.Protect(slot)
	if !ok {
		t.Fatal("expected Protect to succeed")
	}

	// Retire the slot (goes to retirement list).
	if err := sfl.Retire(slot); err != nil {
		t.Fatalf("Retire failed: %v", err)
	}

	// Allocate until we trigger a scan. The protected slot should NOT be reclaimed.
	// Exhaust the pool to trigger scan.
	var allocs [][]byte
	for {
		s, err := sfl.Allocate()
		if err != nil {
			break
		}
		allocs = append(allocs, s)
	}

	// Ensure retiredCount is 0 or the protected slot is still in retirement.
	// If the protected slot were reclaimed, the scan would have pushed it to
	// global FreeList and it would have been allocated above. The protection
	// guarantees it stays in the retirement list.
	n := sfl.retiredCount()
	if n != 1 {
		t.Fatalf("expected 1 protected slot in retirement list, got %d", n)
	}

	// Unprotect and trigger scan again.
	sfl.Unprotect(guard)

	// Deallocate one slot to create space, then allocate — should reclaim.
	for _, s := range allocs {
		sfl.Deallocate(s)
	}
	allocs = nil

	// Now allocate — should reclaim the previously protected slot.
	slot2, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after unprotect failed: %v", err)
	}
	sfl.Deallocate(slot2)
}

func TestHazardDoubleRetire(t *testing.T) {
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
	// Second retire must fail.
	if err := sfl.Retire(slot); err == nil {
		t.Fatal("expected double-retire error")
	}
}

func TestHazardConcurrentProtectRetire(t *testing.T) {
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
	const opsPerGoroutine = 500

	// Pre-allocate slots.
	var slots [][]byte
	for i := 0; i < goroutines*opsPerGoroutine; i++ {
		s, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("pre-allocate failed at %d: %v", i, err)
		}
		slots = append(slots, s)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				slot := slots[base+i]

				// Protect, validate, unprotect.
				guard, ok := sfl.Protect(slot)
				if ok {
					// Simulate reading slot data under protection.
					_ = slot[0]
					sfl.Unprotect(guard)
				}

				// Retire the slot.
				if err := sfl.Retire(slot); err != nil {
					panic(err)
				}
			}
		}(g * opsPerGoroutine)
	}
	wg.Wait()

	// Allocate — should trigger scan and reclaim retired slots.
	for i := 0; i < goroutines*opsPerGoroutine; i++ {
		s, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("re-allocate after concurrent retire+scan failed at %d: %v", i, err)
		}
		s[0] = byte(i)
		sfl.Deallocate(s)
	}
}
