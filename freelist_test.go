package memory

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

// --- Lifecycle tests ---

func TestFreeListBasicLifecycle(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg, 64)
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

	fl, err := NewFreeList(cfg, 64)
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

func TestFreeListPublishesNewSlabWithOneCAS(t *testing.T) {
	cfg := FreeListConfig{
		PoolSize:  4 * 64,
		SlotSize:  64,
		SlabSize:  4 * 64,
		SlabCount: 1,
		Prealloc:  false,
	}
	fl, err := NewFreeList(cfg, 64)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	if _, err := fl.Allocate(); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// One CAS publishes the four-slot chain and one CAS pops its first slot.
	// The tag is the publication/pop sequence counter, so this catches a
	// regression back to one CAS per slot during cold-slab construction.
	if got := unpackTag(fl.head.Load()); got != 2 {
		t.Fatalf("freelist head tag = %d, want 2 after one slab publish and one pop", got)
	}
}

func TestFreeListDoubleFree(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg, 64)
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

	fl, err := NewFreeList(cfg, 64)
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

	fl, err := NewFreeList(cfg, 64)
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

	fl, err := NewFreeList(cfg, 64)
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

// TestPointerMaterializationRegression ensures that the raw pointer manipulation
// correctly yields nil without suffering from Go 1.25 compiler SSA folding bugs.
func TestPointerMaterializationRegression(t *testing.T) {
	// A raw zero value should be safely cast back to a true nil
	var val uint64 = 0
	ptr := unsafe.Pointer(uintptr(val))
	if ptr != nil {
		t.Fatalf("materialized zero was not strictly nil: %p", ptr)
	}

	// Double check unpackPtr wrapper
	uPtr := unpackPtr(0)
	if uPtr != nil {
		t.Fatalf("unpackPtr zero was not strictly nil: %p", uPtr)
	}
}

// --- Zero-allocation verification ---

func TestFreeListZeroHeapAllocs(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg, 64)
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

func TestFreeListAccessors(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.Prealloc = true

	fl, err := NewFreeList(cfg, 64)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()

	if n := fl.PreallocSlabCount(); n != 1 {
		t.Errorf("PreallocSlabCount = %d, want 1", n)
	}
	if n := fl.SlabSize(); n != 64*1024 {
		t.Errorf("SlabSize = %d, want %d", n, 64*1024)
	}
	if n := fl.SlotSize(); n != 64 {
		t.Errorf("SlotSize = %d, want 64", n)
	}
	if n := fl.CasRetries(); n != 0 {
		t.Errorf("CasRetries = %d, want 0", n)
	}

	slots := make([][]byte, 10)
	for i := range slots {
		s, err := fl.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		slots[i] = s
	}
	for _, s := range slots {
		if err := fl.Deallocate(s); err != nil {
			t.Fatalf("Deallocate: %v", err)
		}
	}
	_ = fl.CasRetries()
}

func TestNewFreeListSlabSizeLessThanSlotSize(t *testing.T) {
	cfg := FreeListConfig{
		PoolSize: 1024 * 1024,
		SlotSize: 4096,
		SlabSize: 64, // smaller than SlotSize
	}
	_, err := NewFreeList(cfg, 64)
	if err == nil {
		t.Fatal("expected error for SlabSize < SlotSize")
	}
}

func TestNewFreeListPreallocRollback(t *testing.T) {
	// PoolSize < SlabSize so reserve fails in growSlab during Prealloc.
	cfg := FreeListConfig{
		PoolSize:  64 * 1024, // 64KB
		SlotSize:  64,
		SlabSize:  128 * 1024, // 128KB > PoolSize
		SlabCount: 1,
		Prealloc:  true,
	}
	_, err := NewFreeList(cfg, 64)
	if err == nil {
		t.Fatal("expected error for Prealloc with SlabSize > PoolSize")
	}
}

func TestNewFreeListHugepageValidation(t *testing.T) {
	if HugepageSize == 0 {
		t.Skip("HugepageSize is 0 — huge page validation not testable on this platform")
	}
	cfg := FreeListConfig{
		PoolSize:     64 * 1024 * 1024,
		SlotSize:     64,
		SlabSize:     HugepageSize + 1, // not a multiple
		SlabCount:    1,
		UseHugePages: true,
	}
	_, err := NewFreeList(cfg, 64)
	if err == nil {
		t.Fatal("expected error for SlabSize not a multiple of HugepageSize")
	}
}

func TestNewFreeListSlotSizeMinimum(t *testing.T) {
	// SlotSize < 32 should be clamped to 32, not error.
	cfg := FreeListConfig{
		PoolSize: 64 * 1024,
		SlotSize: 16,
		SlabSize: 4096,
	}
	fl, err := NewFreeList(cfg, 64)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	defer fl.Free()
	if fl.cfg.SlotSize != 64 {
		t.Errorf("SlotSize = %d, want 64 (clamped from 16)", fl.cfg.SlotSize)
	}
}

func TestFreeListGenerationTableDoesNotScaleGoHeap(t *testing.T) {
	// A 512 MiB/64 B FreeList needs an 64 MiB generation table. That table is
	// allocator metadata and must be mmap-backed: putting it on the Go heap
	// would recreate the exact pressure the allocator is intended to avoid.
	cfg := FreeListConfig{
		PoolSize:  512 * 1024 * 1024,
		SlotSize:  64,
		SlabSize:  2 * 1024 * 1024,
		SlabCount: 1,
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	fl, err := NewFreeList(cfg, 64)
	if err != nil {
		t.Fatalf("NewFreeList: %v", err)
	}
	if fl.slotGenBase == 0 {
		t.Fatal("slot generation table was not mmap-backed")
	}
	if want := cfg.PoolSize / cfg.SlotSize * uint64(unsafe.Sizeof(atomic.Uint64{})); fl.slotGenSize != want {
		t.Fatalf("slot generation table size = %d, want %d", fl.slotGenSize, want)
	}

	generationTableSize := fl.slotGenSize
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(fl)
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > 2*1024*1024 {
		_ = fl.Free()
		t.Fatalf("NewFreeList added %d Go-heap bytes for a %d-byte generation table", delta, generationTableSize)
	}

	if err := fl.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if fl.slotGenBase != 0 || fl.slotGenSize != 0 || fl.slotGenLen != 0 {
		t.Fatal("Free did not release the mmap-backed generation table")
	}
}
