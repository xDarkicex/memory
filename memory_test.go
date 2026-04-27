package memory

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestNewPoolDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.PoolSize == 0 {
		t.Fatal("default PoolSize should be non-zero")
	}
	if cfg.SlabSize == 0 {
		t.Fatal("default SlabSize should be non-zero")
	}
	if cfg.SlabCount == 0 {
		t.Fatal("default SlabCount should be non-zero")
	}
}

func TestNewPoolInvalidSize(t *testing.T) {
	_, err := NewPool(AllocatorConfig{PoolSize: 0, SlabSize: 1024})
	if err != nil {
		t.Fatalf("expected no error for PoolSize=0 (uses default), got: %v", err)
	}

	_, err = NewPool(AllocatorConfig{SlabSize: 0})
	if err != nil {
		t.Fatalf("expected no error for SlabSize=0 (uses default), got: %v", err)
	}
}

func TestAllocateZeroSize(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	_, err = pool.Allocate(0)
	if err != ErrInvalidSize {
		t.Fatalf("expected ErrInvalidSize, got: %v", err)
	}
}

func TestAllocateBasic(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(64)
	if err != nil {
		t.Fatalf("Allocate(64) failed: %v", err)
	}
	if len(data) != 64 {
		t.Fatalf("expected len(data)=64, got %d", len(data))
	}

	// Write and read to verify memory is valid
	for i := range data {
		data[i] = byte(i)
	}
	for i := range data {
		if data[i] != byte(i) {
			t.Fatalf("data[%d] mismatch: got %02x, want %02x", i, data[i], byte(i))
		}
	}

	stats := pool.Stats()
	if stats.Allocated == 0 {
		t.Fatal("Allocated should be non-zero after allocation")
	}
	if stats.Reserved == 0 {
		t.Fatal("Reserved should be non-zero after allocation")
	}
}

func TestPoolExhausted(t *testing.T) {
	// Use a tiny pool that can't satisfy large allocations
	cfg := AllocatorConfig{
		PoolSize:  64 * 1024, // 64KB
		SlabSize:  32 * 1024, // 32KB
		SlabCount: 1,         // only 1 slab = 32KB max
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	// Allocate within single slab (32KB < 64KB pool)
	_, err = pool.Allocate(16 * 1024)
	if err != nil {
		t.Fatalf("Allocate(16KB) failed: %v", err)
	}

	// Try to allocate more than remaining pool size
	_, err = pool.Allocate(128 * 1024) // exceeds PoolSize
	if err != ErrPoolExhausted {
		t.Fatalf("expected ErrPoolExhausted for oversized alloc, got: %v", err)
	}
}

func TestPoolReset(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	// Allocate some memory
	ptr1, err := pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate(1024) failed: %v", err)
	}

	statsBefore := pool.Stats()
	if statsBefore.Allocated == 0 {
		t.Fatal("Allocated should be non-zero before Reset")
	}

	// Reset the pool
	pool.Reset()

	// After Reset, stats should reflect empty state
	statsAfter := pool.Stats()
	if statsAfter.Allocated != 0 {
		t.Fatalf("Allocated should be 0 after Reset, got %d", statsAfter.Allocated)
	}
	if statsAfter.Reserved != 0 {
		t.Fatalf("Reserved should be 0 after Reset, got %d", statsAfter.Reserved)
	}

	// Old allocation should be invalid (would segfault if accessed)
	// We verify by allocating new memory at the same logical position
	ptr2, err := pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate after Reset failed: %v", err)
	}
	// ptr2 may or may not be the same address - that's fine
	_ = unsafe.Pointer(&ptr2[0])
	_ = unsafe.Pointer(&ptr1[0])
}

func TestNewArenaBasic(t *testing.T) {
	arena, err := NewArena(1024 * 1024) // 1MB arena
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	ptr, err := arena.Alloc(256)
	if err != nil {
		t.Fatalf("Arena.Alloc(256) failed: %v", err)
	}
	if ptr == unsafe.Pointer(nil) {
		t.Fatal("Alloc returned nil pointer")
	}

	remaining := arena.Remaining()
	if remaining >= 1024*1024 {
		t.Fatalf("Remaining() should be less than arena size after allocation: %d", remaining)
	}
}

func TestArenaZeroSize(t *testing.T) {
	arena, err := NewArena(1024 * 1024)
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	_, err = arena.Alloc(0)
	if err != ErrInvalidSize {
		t.Fatalf("expected ErrInvalidSize for zero size, got: %v", err)
	}
}

func TestArenaExhausted(t *testing.T) {
	arena, err := NewArena(512) // 512 byte arena
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	// Allocate half the arena
	_, err = arena.Alloc(256)
	if err != nil {
		t.Fatalf("Alloc(256) failed: %v", err)
	}

	// Allocate remaining
	_, err = arena.Alloc(256)
	if err != nil {
		t.Fatalf("Alloc(256) second failed: %v", err)
	}

	// Try to allocate beyond capacity
	_, err = arena.Alloc(1)
	if err != ErrArenaExhausted {
		t.Fatalf("expected ErrArenaExhausted, got: %v", err)
	}
}

func TestArenaReset(t *testing.T) {
	arena, err := NewArena(1024 * 1024)
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	// Allocate
	_, err = arena.Alloc(512)
	if err != nil {
		t.Fatalf("Alloc(512) failed: %v", err)
	}

	remainingBefore := arena.Remaining()

	// Reset
	arena.Reset()

	remainingAfter := arena.Remaining()
	if remainingAfter <= remainingBefore {
		t.Fatalf("Remaining() should increase after Reset: before=%d, after=%d", remainingBefore, remainingAfter)
	}
	if remainingAfter != 1024*1024 {
		t.Fatalf("Remaining() should be full arena size after Reset: got %d, want %d", remainingAfter, 1024*1024)
	}
}

func TestArenaFree(t *testing.T) {
	arena, err := NewArena(1024 * 1024)
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}

	// Allocate before free
	ptr, err := arena.Alloc(256)
	if err != nil {
		t.Fatalf("Alloc(256) failed: %v", err)
	}

	// Free the arena
	err = arena.Free()
	if err != nil {
		t.Fatalf("Arena.Free() failed: %v", err)
	}

	// Remaining should be 0 after free (len(nil) == 0)
	remaining := arena.Remaining()
	if remaining != 0 {
		t.Fatalf("Remaining() should be 0 after Free, got %d", remaining)
	}

	// Subsequent Alloc should fail
	_, err = arena.Alloc(256)
	if err != ErrArenaExhausted {
		t.Fatalf("expected ErrArenaExhausted after Free, got: %v", err)
	}

	_ = ptr
}

func TestArenaUseAfterFree(t *testing.T) {
	arena, err := NewArena(1024 * 1024)
	if err != nil {
		t.Fatalf("NewArena failed: %v", err)
	}

	arena.Free()

	// Alloc after Free should handle nil data gracefully
	ptr, err := arena.Alloc(256)
	if ptr != unsafe.Pointer(nil) {
		t.Fatal("Alloc after Free should return nil pointer")
	}
	if err != ErrArenaExhausted {
		t.Fatalf("expected ErrArenaExhausted after Free, got: %v", err)
	}
}

func TestPoolStats(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 4

	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	stats := pool.Stats()
	if stats.SlabSize != cfg.SlabSize {
		t.Fatalf("SlabSize mismatch: got %d, want %d", stats.SlabSize, cfg.SlabSize)
	}
	if stats.Align == 0 {
		t.Fatal("Align should be non-zero")
	}
	if stats.SlabCount != 0 {
		t.Fatalf("SlabCount should be 0 before first allocation, got %d", stats.SlabCount)
	}

	// Allocate to trigger slab creation
	_, err = pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate(1024) failed: %v", err)
	}

	stats = pool.Stats()
	if stats.SlabCount <= 0 {
		t.Fatal("SlabCount should be > 0 after allocation")
	}
}

func TestPoolLargeAllocation(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  4 * 1024 * 1024, // 4MB pool
		SlabSize:  1024 * 1024,     // 1MB slabs
		SlabCount: 2,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	// Allocate more than slab size (large allocation)
	data, err := pool.Allocate(2 * 1024 * 1024) // 2MB
	if err != nil {
		t.Fatalf("Allocate(2MB) failed: %v", err)
	}
	if len(data) != 2*1024*1024 {
		t.Fatalf("expected len(data)=2MB, got %d", len(data))
	}

	stats := pool.Stats()
	if stats.Committed == 0 {
		t.Fatal("Committed should be non-zero for large allocation")
	}
}

func TestHugepageSizeAutodetect(t *testing.T) {
	// HugepageSize should be set on Linux
	if runtime.GOOS == "linux" {
		if HugepageSize == 0 {
			t.Log("HugepageSize is 0 on Linux - may indicate autodetect failed or system has no huge pages")
		}
		// On x86_64, should be 2MB
		if runtime.GOARCH == "amd64" && HugepageSize != 2*1024*1024 {
			t.Logf("HugepageSize on amd64 is %d, expected 2MB for most systems", HugepageSize)
		}
	}
}

func TestPoolPrealloc(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  256 * 1024,
		SlabSize:  64 * 1024,
		SlabCount: 2,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool with Prealloc failed: %v", err)
	}
	defer pool.Reset()

	stats := pool.Stats()
	if stats.SlabCount != 2 {
		t.Fatalf("expected 2 slabs after Prealloc, got %d", stats.SlabCount)
	}
	if stats.Reserved == 0 {
		t.Fatal("Reserved should be non-zero after Prealloc")
	}

	// Allocate within preallocated slabs
	data, err := pool.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate(1024) failed: %v", err)
	}
	if len(data) != 1024 {
		t.Fatalf("expected len(data)=1024, got %d", len(data))
	}
	_ = data
}

func TestPoolPreallocRollback(t *testing.T) {
	cfg := AllocatorConfig{
		PoolSize:  96 * 1024,  // less than 2 * 64KB slabs
		SlabSize:  64 * 1024,
		SlabCount: 2,
		Prealloc:  true,
	}
	_, err := NewPool(cfg)
	if err != ErrPoolExhausted {
		t.Fatalf("expected ErrPoolExhausted for prealloc exceeding PoolSize, got: %v", err)
	}
}

func TestHintNormal(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Hint with HintNormal should not panic
	Hint(HintNormal, unsafe.Pointer(&data[0]), len(data))
}

func TestHintWillNeed(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Hint with HintWillNeed should not panic
	Hint(HintWillNeed, unsafe.Pointer(&data[0]), len(data))
}

func TestHintDontNeed(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Hint with HintDontNeed should not panic
	Hint(HintDontNeed, unsafe.Pointer(&data[0]), len(data))
}

func TestHintZeroLength(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Hint with zero length should not panic
	Hint(HintNormal, unsafe.Pointer(&data[0]), 0)
	Hint(HintWillNeed, unsafe.Pointer(&data[0]), 0)
	Hint(HintDontNeed, unsafe.Pointer(&data[0]), 0)
}

func TestZeroMemory(t *testing.T) {
	mem := make([]byte, 64)
	for i := range mem {
		mem[i] = 0xFF
	}

	ZeroMemory(unsafe.Pointer(&mem[0]), 64)

	for i := range mem {
		if mem[i] != 0 {
			t.Fatalf("ZeroMemory failed at byte %d: got %02x, want 0", i, mem[i])
		}
	}
}

func TestReadMemStats(t *testing.T) {
	stats := ReadMemStats()
	if stats.Total == 0 {
		t.Log("Total is 0 - may be expected for fresh process")
	}
	// These are Go heap metrics; values depend on runtime state
	// Just verify they don't panic
	_ = stats.Used
	_ = stats.Free
}

func TestReadGCStats(t *testing.T) {
	stats := ReadGCStats()
	// GC stats should be populated after runtime.ReadMemStats
	// Just verify they don't panic
	_ = stats.NumGC
}

func TestReadProfile(t *testing.T) {
	profile := ReadProfile()
	// Should be populated
	if profile.Alloc == 0 && profile.TotalAlloc == 0 {
		t.Log("Profile may be zero for fresh allocation")
	}
}
