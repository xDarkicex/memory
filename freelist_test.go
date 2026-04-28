package memory

import (
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
	if count == 0 {
		t.Error("expected at least one allocation before exhaustion")
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

	// Double-free detection is a hardening TODO.
	// Currently the second Deallocate silently corrupts the freelist.
	// When implemented, this test should expect ErrDoubleDeallocation.
	slot, _ := fl.Allocate()
	if err := fl.Deallocate(slot); err != nil {
		t.Fatalf("first Deallocate: %v", err)
	}
	// Second deallocate: currently succeeds (sharp edge), future: error.
	_ = fl.Deallocate(slot)
	t.Log("double-free detection is a hardening TODO — second deallocate currently succeeds")
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

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				slot, err := fl.Allocate()
				if err != nil {
					t.Errorf("Allocate: %v", err)
					return
				}
				// Minimal use: write goroutine tag.
				if len(slot) > 0 {
					slot[0] = byte(g)
				}
				if err := fl.Deallocate(slot); err != nil {
					t.Errorf("Deallocate: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// After all deallocations, the freelist should be full again.
	// The allocated count should be 0 since everything was returned.
	stats := fl.Stats()
	if stats.Allocated != 0 {
		t.Errorf("after concurrent cycle: allocated = %d, want 0", stats.Allocated)
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

// --- Benchmarks ---

func BenchmarkFreeListHotPath(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		slot, _ := fl.Allocate()
		fl.Deallocate(slot)
	}
}

func BenchmarkFreeListAllocateOnly(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		slot, err := fl.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		fl.Deallocate(slot)
	}
}

func BenchmarkFreeListConcurrent(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := fl.Allocate()
			if err != nil {
				b.Fatal(err)
			}
			fl.Deallocate(slot)
		}
	})
}

// Benchmark comparison: FreeList vs Pool for fixed-size workload.
func BenchmarkFreeListVsPool_64B(b *testing.B) {
	// FreeList
	b.Run("FreeList", func(b *testing.B) {
		cfg := DefaultFreeListConfig()
		cfg.PoolSize = 64 * 1024 * 1024
		cfg.SlotSize = 64
		cfg.SlabSize = 1024 * 1024
		cfg.Prealloc = true

		fl, _ := NewFreeList(cfg)
		defer fl.Free()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			slot, _ := fl.Allocate()
			fl.Deallocate(slot)
		}
	})

	// Pool (bulk Reset equivalent)
	b.Run("Pool", func(b *testing.B) {
		cfg := AllocatorConfig{
			PoolSize:  64 * 1024 * 1024,
			SlabSize:  1024 * 1024,
			SlabCount: 16,
			Prealloc:  true,
		}
		pool, _ := NewPool(cfg)
		defer pool.Reset()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			_, err := pool.Allocate(64)
			if err != nil {
				b.Fatal(err)
			}
			// Pool has no Deallocate — can't free individually.
			// This benchmark is here for structural comparison only.
		}
	})
}
