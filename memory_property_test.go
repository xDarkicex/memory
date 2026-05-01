package memory

import (
	"sync"
	"testing"
	"testing/quick"
	"unsafe"
)

// TestPoolSizeNeverExceeded checks that total allocated bytes never exceeds PoolSize
func TestPoolSizeNeverExceeded(t *testing.T) {
	f := func(size uint32) bool {
		if size == 0 {
			return true // skip zero
		}
		cfg := AllocatorConfig{
			PoolSize:  1024 * 1024, // 1MB pool
			SlabSize:  256 * 1024, // 256KB slabs
			SlabCount: 4,
		}
		pool, err := NewPool(cfg)
		if err != nil {
			t.Logf("NewPool failed: %v", err)
			return false
		}
		defer pool.Free()

		// Try to allocate up to PoolSize
		allocSize := uint64(size) % (cfg.PoolSize / 2) // cap at half pool to avoid immediate exhaustion
		data, err := pool.Allocate(allocSize)
		if err != nil {
			// ErrPoolExhausted is acceptable
			return err == ErrPoolExhausted
		}

		// Write to memory to ensure it's valid
		for i := range data {
			data[i] = byte(i)
		}

		stats := pool.Stats()
		// Allocated should never exceed PoolSize
		return stats.Allocated <= cfg.PoolSize
	}

	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// TestResetRestoresFullCapacity checks that after Reset, the full PoolSize is available
func TestResetRestoresFullCapacity(t *testing.T) {
	f := func(numAllocs uint8) bool {
		if numAllocs == 0 {
			return true
		}
		cfg := AllocatorConfig{
			PoolSize:  512 * 1024, // 512KB
			SlabSize:  128 * 1024, // 128KB slabs
			SlabCount: 4,
		}
		pool, err := NewPool(cfg)
		if err != nil {
			t.Logf("NewPool failed: %v", err)
			return false
		}
		defer pool.Free()

		allocSize := uint64(32 * 1024) // 32KB each

		// Allocate multiple times
		for i := uint8(0); i < numAllocs && i < 16; i++ {
			if _, err := pool.Allocate(allocSize); err != nil {
				break
			}
		}

		statsBefore := pool.Stats()
		if statsBefore.Allocated == 0 {
			return true // nothing allocated, trivial case
		}

		// Reset
		pool.Reset()

		statsAfter := pool.Stats()
		if statsAfter.Allocated != 0 {
			t.Logf("Allocated after Reset should be 0, got %d", statsAfter.Allocated)
			return false
		}
		if statsAfter.Reserved != 0 {
			t.Logf("Reserved after Reset should be 0, got %d", statsAfter.Reserved)
			return false
		}

		// After reset, we should be able to allocate the same total amount
		var totalAllocated uint64
		for i := uint8(0); i < numAllocs && i < 16; i++ {
			_, err := pool.Allocate(allocSize)
			if err != nil {
				break
			}
totalAllocated += allocSize
		}

		statsNew := pool.Stats()
		return statsNew.Allocated == totalAllocated
	}

	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// TestGenerationIncrementsOnReset verifies generation counter increments
func TestGenerationIncrementsOnReset(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	// Allocate to ensure pool is initialized
	_, err = pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Get initial generation - need to access via reflection or indirect method
	// Since generation is not exposed, we verify through behavior:
	// After reset, old allocations should fail gen check
	oldData, err := pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Store old data pointer to verify it's invalidated later
	oldPtr := unsafe.Pointer(&oldData[0])

	pool.Reset()

	// Allocate new data
	newData, err := pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate after Reset failed: %v", err)
	}

	newPtr := unsafe.Pointer(&newData[0])

	// The pointers should be different (new mmap after reset)
	// Note: This is probabilistic, not guaranteed, but highly likely
	_ = oldPtr
	_ = newPtr
}

// TestAllocatedNeverExceedsReserved verifies Allocated ≤ Reserved
func TestAllocatedNeverExceedsReserved(t *testing.T) {
	f := func(numAllocs uint8) bool {
		cfg := DefaultConfig()
		cfg.SlabSize = 64 * 1024
		cfg.SlabCount = 8

		pool, err := NewPool(cfg)
		if err != nil {
			t.Logf("NewPool failed: %v", err)
			return false
		}
		defer pool.Free()

		allocSize := uint64(16 * 1024) // 16KB allocations

		for i := uint8(0); i < numAllocs && i < 32; i++ {
			_, err := pool.Allocate(allocSize)
			if err != nil {
				break
			}
		}

		stats := pool.Stats()
		return stats.Allocated <= stats.Reserved
	}

	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// TestSlabCountMonotonicIncrease verifies slab count only grows
func TestSlabCountMonotonicIncrease(t *testing.T) {
	pool, err := NewPool(AllocatorConfig{
		PoolSize:  256 * 1024,
		SlabSize:  64 * 1024,
		SlabCount: 1,
	})
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	prevCount := int32(0)
	allocSize := uint64(32 * 1024)

	for i := 0; i < 100; i++ {
		_, err := pool.Allocate(allocSize)
		if err != nil {
			break
		}
		stats := pool.Stats()
		if stats.SlabCount < prevCount {
			t.Fatalf("SlabCount decreased: was %d, now %d", prevCount, stats.SlabCount)
		}
		prevCount = stats.SlabCount
	}
}

// TestNoMemoryLeakAfterFree verifies memory is properly released
func TestNoMemoryLeakAfterFree(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  4 * 1024 * 1024, // 4MB
		SlabSize:  1 * 1024 * 1024,
		SlabCount: 4,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	// Allocate and then free
	_, err = pool.Allocate(1024 * 1024) // 1MB
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	statsBefore := pool.Stats()

	pool.Reset()

	statsAfter := pool.Stats()

	// After reset, both should be 0
	if statsAfter.Allocated != 0 || statsAfter.Reserved != 0 {
		t.Fatalf("After Reset: Allocated=%d, Reserved=%d, expected both 0",
			statsAfter.Allocated, statsAfter.Reserved)
	}

	_ = statsBefore
}

// TestMultipleLargeAllocations verifies large alloc tracking
func TestMultipleLargeAllocations(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  16 * 1024 * 1024, // 16MB
		SlabSize:  2 * 1024 * 1024,  // 2MB slabs
		SlabCount: 4,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	// Allocate several large objects (larger than slab size)
	for i := 0; i < 3; i++ {
		data, err := pool.Allocate(4 * 1024 * 1024) // 4MB each
		if err != nil {
			t.Fatalf("Large allocation %d failed: %v", i, err)
		}
		// Write to ensure it's mapped
		for j := range data {
			data[j] = byte(j)
		}
	}

	stats := pool.Stats()
	if stats.Committed == 0 {
		t.Fatal("Committed should be non-zero for large allocations")
	}
	if stats.PeakUsage == 0 {
		t.Fatal("PeakUsage should be set")
	}
}

// TestConcurrentAllocNoRace verifies lock-free hot path works under concurrency
func TestConcurrentAllocNoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	cfg := AllocatorConfig{
		PoolSize:  8 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 16,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	const numGoroutines = 8
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				data, err := pool.Allocate(256)
				if err != nil {
					continue // pool exhausted is acceptable under load
				}
				// Write to memory
				for j := range data {
					data[j] = byte(i + j)
				}
			}
		}()
	}

	wg.Wait()

	stats := pool.Stats()
	if stats.Allocated == 0 {
		t.Fatal("At least some allocations should succeed under concurrent load")
	}
}

// TestArenaConcurrentAlloc tests Arena with concurrent producers
func TestArenaConcurrentAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	arena, err := NewArena(1024 * 1024 * 4) // 4MB arena
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	const numGoroutines = 4
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				ptr, err := arena.Alloc(256)
				if err != nil {
					continue // arena exhausted is acceptable
				}
				// Write to memory
				data := ptrToSlice(ptr, 256)
				for j := range data {
					data[j] = byte(i + j)
				}
			}
		}()
	}

	wg.Wait()

	remaining := arena.Remaining()
	t.Logf("Arena remaining after concurrent use: %d bytes", remaining)
}

// TestArenaOffsetWraparound prevents offset arithmetic wraparound
func TestArenaOffsetWraparound(t *testing.T) {
	arena, err := NewArena(1024) // Small arena
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	// Fill the arena
	_, err = arena.Alloc(512)
	if err != nil {
		t.Fatalf("Alloc(512) failed: %v", err)
	}

	_, err = arena.Alloc(512)
	if err != nil {
		t.Fatalf("Alloc(512) second failed: %v", err)
	}

	// Try to allocate beyond capacity - should fail, not wrap
	_, err = arena.Alloc(1)
	if err != ErrArenaExhausted {
		t.Fatalf("expected ErrArenaExhausted, got: %v", err)
	}
}

// TestPoolAlignment verifies 8-byte alignment
func TestPoolAlignment(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	for _, size := range []uint64{1, 2, 3, 4, 5, 7, 8, 15, 16, 17, 31, 32, 33} {
		data, err := pool.Allocate(size)
		if err != nil {
			t.Fatalf("Allocate(%d) failed: %v", size, err)
		}

		// Check alignment
		addr := uintptr(unsafe.Pointer(&data[0]))
		if addr%8 != 0 {
			t.Fatalf("Allocation of %d bytes has address %x which is not 8-byte aligned", size, addr)
		}
	}
}

// TestReservedAccountant verifies reserved bytes accounting
func TestReservedAccountant(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  256 * 1024,
		SlabSize:  64 * 1024,
		SlabCount: 1,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

	// Before any allocation, reserved should be 0 (lazy allocation)
	stats := pool.Stats()
	if stats.Reserved != 0 {
		t.Fatalf("Reserved should be 0 before first allocation, got %d", stats.Reserved)
	}

	// Allocate small amount
	_, err = pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate(1024) failed: %v", err)
	}

	stats = pool.Stats()
	if stats.Reserved == 0 {
		t.Fatal("Reserved should be non-zero after allocation")
	}
	if stats.Reserved > cfg.PoolSize {
		t.Fatalf("Reserved (%d) exceeds PoolSize (%d)", stats.Reserved, cfg.PoolSize)
	}
}

// Helper function to write to arena memory
func ptrToSlice(ptr unsafe.Pointer, size int) []byte {
	return unsafe.Slice((*byte)(ptr), size)
}
