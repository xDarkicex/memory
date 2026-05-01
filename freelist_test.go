package memory

import (
	"fmt"
	"sync"
	"testing"
)

// --- Lifecycle tests ---

func TestFreeListBasicLifecycle(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	// Allocate all slots in the pre-allocated slab.
	slotsPerSlab := int(cfg.SlabSize / fl.cfg.SlotSize)
	allocated := make([][]byte, 0, slotsPerSlab)

	for i := 0; i < slotsPerSlab; i++ {
		slot, err := fl.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		if len(slot) != int(fl.cfg.SlotSize) {
			t.Fatalf("slot %d: got len %d, want %d", i, len(slot), fl.cfg.SlotSize)
		}
		// Write a pattern to verify the memory is usable.
		for j := range slot {
			slot[j] = byte(i & 0xFF)
		}
		allocated = append(allocated, slot)
	}

	stats := fl.Stats()
	if stats.Allocated != uint64(slotsPerSlab)*fl.cfg.SlotSize {
		t.Errorf("allocated = %d, want %d", stats.Allocated, uint64(slotsPerSlab)*fl.cfg.SlotSize)
	}

	// Deallocate half.
	for i := 0; i < slotsPerSlab/2; i++ {
		if err := fl.Deallocate(allocated[i]); err != nil {
			t.Fatalf("Deallocate %d: %v", i, err)
		}
	}

	// Re-allocate.
	for i := 0; i < slotsPerSlab/2; i++ {
		slot, err := fl.Allocate()
		if err != nil {
			t.Fatalf("re-Allocate %d: %v", i, err)
		}
		if len(slot) != int(fl.cfg.SlotSize) {
			t.Fatalf("re-alloc slot %d: got len %d, want %d", i, len(slot), fl.cfg.SlotSize)
		}
	}
}

func TestFreeListExhaustion(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 // 64KB
	cfg.SlotSize = 64
	cfg.SlabSize = 4 * 1024 // 4KB slabs
	cfg.SlabCount = 1
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	// Allocate until exhaustion.
	var count int
	for {
		_, err := fl.Allocate()
		if err == ErrFreelistExhausted {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count++
	}
	// PoolSize=64KB, SlotSize=64B → exactly 1024 slots.
	expected := int(cfg.PoolSize / cfg.SlotSize)
	if count != expected {
		t.Errorf("exhaustion count = %d, want %d", count, expected)
	}
}

func TestFreeListDoubleFree(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	slot, err := fl.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// First deallocate should succeed.
	if err := fl.Deallocate(slot); err != nil {
		t.Fatalf("first Deallocate: %v", err)
	}

	// Second deallocate of the same slot must return ErrDoubleDeallocation.
	if err := fl.Deallocate(slot); err != ErrDoubleDeallocation {
		t.Errorf("second Deallocate: got %v, want ErrDoubleDeallocation", err)
	}

	// Verify the freelist is not corrupted: allocate a slot and use it.
	newSlot, err := fl.Allocate()
	if err != nil {
		t.Fatalf("post-double-free Allocate: %v", err)
	}
	if len(newSlot) != 64 {
		t.Errorf("post-double-free slot len = %d, want 64", len(newSlot))
	}
	fl.Deallocate(newSlot)
}

func TestFreeListInvalidDeallocation(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	if err := fl.Deallocate(nil); err != ErrInvalidDeallocation {
		t.Errorf("nil slice: got %v, want ErrInvalidDeallocation", err)
	}
	if err := fl.Deallocate([]byte{}); err != ErrInvalidDeallocation {
		t.Errorf("empty slice: got %v, want ErrInvalidDeallocation", err)
	}
	// External (heap-allocated) pointer must be rejected.
	external := make([]byte, 64)
	if err := fl.Deallocate(external); err != ErrInvalidDeallocation {
		t.Errorf("external slice: got %v, want ErrInvalidDeallocation", err)
	}

	// Unaligned pointer within a valid slab must be rejected.
	slot, err := fl.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	unaligned := slot[1:] // offset by 1 byte from valid slot boundary
	if err := fl.Deallocate(unaligned); err != ErrInvalidDeallocation {
		t.Errorf("unaligned pointer: got %v, want ErrInvalidDeallocation", err)
	}
	// Return the properly-aligned slot so it doesn't leak.
	fl.Deallocate(slot)
}

func TestFreeListReset(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	// Allocate some slots.
	for i := 0; i < 10; i++ {
		fl.Allocate()
	}

	stats := fl.Stats()
	if stats.Allocated == 0 {
		t.Error("expected non-zero allocated before Reset")
	}

	fl.Reset()

	stats = fl.Stats()
	if stats.Allocated != 0 {
		t.Errorf("after Reset: allocated = %d, want 0", stats.Allocated)
	}
	if stats.Reserved != 0 {
		t.Errorf("after Reset: reserved = %d, want 0", stats.Reserved)
	}
}

// --- Concurrent tests ---

func TestFreeListConcurrent(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 16 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	const goroutines = 8
	const opsPerGoroutine = 1000

	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				slot, err := fl.Allocate()
				if err != nil {
					select {
					case errCh <- fmt.Errorf("goroutine %d Allocate %d: %v", id, i, err):
					default:
					}
					return
				}
				if len(slot) > 0 {
					slot[0] = byte(id)
				}
				if err := fl.Deallocate(slot); err != nil {
					select {
					case errCh <- fmt.Errorf("goroutine %d Deallocate %d: %v", id, i, err):
					default:
					}
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for e := range errCh {
		t.Error(e)
	}

	stats := fl.Stats()
	if stats.Allocated != 0 {
		t.Errorf("after concurrent cycle: allocated = %d, want 0", stats.Allocated)
	}

	// Verify the freelist is still usable after the concurrent cycle.
	for i := 0; i < goroutines*opsPerGoroutine; i++ {
		if _, err := fl.Allocate(); err != nil {
			t.Fatalf("post-cycle re-allocate %d failed: %v", i, err)
		}
	}
	stats = fl.Stats()
	want := uint64(goroutines*opsPerGoroutine) * fl.cfg.SlotSize
	if stats.Allocated != want {
		t.Errorf("post-cycle allocated = %d, want %d", stats.Allocated, want)
	}
}



// --- Zero-allocation verification ---

func TestFreeListZeroHeapAllocs(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			slot, _ := fl.Allocate()
			fl.Deallocate(slot)
		}
	})

	if result.AllocsPerOp() > 0 {
		t.Errorf("Allocate/Deallocate cycle: got %d allocs/op, want 0", result.AllocsPerOp())
	}
}

