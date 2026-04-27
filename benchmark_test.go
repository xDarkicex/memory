package memory

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

// benchmarkAllocBatch runs allocation benchmarks with periodic pool reset
// to avoid PoolSize exhaustion. batchSize controls how many allocations
// before Reset() is called.
func benchmarkAllocBatch(b *testing.B, pool *Pool, allocSize uint64, batchSize int) {
	b.ReportAllocs()
	b.SetBytes(int64(allocSize))

	var sink byte
	b.ResetTimer()

	for i := 0; i < b.N; i += batchSize {
		batch := batchSize
		if i+batch > b.N {
			batch = b.N - i
		}
		for j := 0; j < batch; j++ {
			data, err := pool.Allocate(allocSize)
			if err != nil {
				b.Fatalf("Allocate failed: %v (i=%d, j=%d)", err, i, j)
			}
			sink = data[0]
		}
		// Report internal fragmentation before reclaim
		s := pool.Stats()
		if s.Reserved > 0 {
			b.ReportMetric(float64(s.Allocated)/float64(s.Reserved)*100, "util-%")
			b.ReportMetric(float64(s.Reserved-s.Allocated), "waste-B/op")
		}
		pool.Reset()
	}
	_ = sink
}

// BenchmarkPoolAllocateHotPath measures O(1) allocation when slabs are pre-allocated.
func BenchmarkPoolAllocateHotPath(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  256 * 1024 * 1024, // 256MB — plenty for batchSize=1000 × 64B
		SlabSize:  256 * 1024,        // 256KB slabs
		SlabCount: 16,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	benchmarkAllocBatch(b, pool, 64, 1000)
}

// BenchmarkPoolAllocateSlowPath measures allocation when we must scan for a fitting slab.
func BenchmarkPoolAllocateSlowPath(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  256 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	// Fill first slab to force slow-path scanning (200KB used, ~56KB remaining)
	_, err = pool.Allocate(200 * 1024)
	if err != nil {
		b.Fatalf("Setup failed: %v", err)
	}

	benchmarkAllocBatch(b, pool, 100*1024, 100) // 100 × 100KB = 10MB per batch
}

// BenchmarkPoolAllocateGrowPath measures allocation that triggers slab growth.
func BenchmarkPoolAllocateGrowPath(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  256 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 1, // Start with minimal slabs to force growth
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	benchmarkAllocBatch(b, pool, 256*1024, 50) // 50 × 256KB = 12.8MB per batch
}

// BenchmarkPoolResetDuration measures Reset() time across N slabs.
func BenchmarkPoolResetDuration(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  32 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 64, // 16MB total
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	// Pre-fill to create all slabs
	for i := 0; i < 64; i++ {
		_, err := pool.Allocate(256 * 1024)
		if err != nil {
			b.Fatalf("Setup failed: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Reset()
		for j := 0; j < 64; j++ {
			_, _ = pool.Allocate(256 * 1024)
		}
		s := pool.Stats()
		if s.Reserved > 0 {
			b.ReportMetric(float64(s.Allocated)/float64(s.Reserved)*100, "util-%")
		}
	}
}

// BenchmarkPoolResetCost measures the absolute cost of Reset() with N slabs.
func BenchmarkPoolResetCost(b *testing.B) {
	slabCounts := []int{4, 16, 64, 256}

	for _, slabCount := range slabCounts {
		b.Run(fmt.Sprintf("Slabs=%d", slabCount), func(b *testing.B) {
			cfg := AllocatorConfig{
				PoolSize:  uint64(slabCount) * 256 * 1024,
				SlabSize:  256 * 1024,
				SlabCount: slabCount,
				Prealloc:  true,
			}
			pool, err := NewPool(cfg)
			if err != nil {
				b.Fatalf("NewPool failed: %v", err)
			}

			// Fill all slabs
			for i := 0; i < slabCount; i++ {
				_, err := pool.Allocate(256 * 1024)
				if err != nil {
					b.Fatalf("Setup failed: %v", err)
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				pool.Reset()
				for j := 0; j < slabCount; j++ {
					_, _ = pool.Allocate(256 * 1024)
				}
				s := pool.Stats()
				if s.Reserved > 0 {
					b.ReportMetric(float64(s.Allocated)/float64(s.Reserved)*100, "util-%")
				}
			}
			pool.Reset()
		})
	}
}

// BenchmarkArenaAllocThroughput measures Arena.Alloc throughput.
func BenchmarkArenaAllocThroughput(b *testing.B) {
	const arenaSize = 64 * 1024 * 1024
	const allocSize = 64
	const batchSize = 10000

	arena, err := NewArena(arenaSize)
	if err != nil {
		b.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	b.ReportAllocs()
	b.SetBytes(allocSize)

	var sink byte
	b.ResetTimer()

	for i := 0; i < b.N; i += batchSize {
		batch := batchSize
		if i+batch > b.N {
			batch = b.N - i
		}
		arena.Reset()
		for j := 0; j < batch; j++ {
			ptr, err := arena.Alloc(allocSize)
			if err != nil {
				b.Fatal(err)
			}
			sink = *(*byte)(ptr)
		}
		rem := arena.Remaining()
		used := float64(uint64(len(arena.data))-rem) / float64(len(arena.data)) * 100
		b.ReportMetric(used, "util-%")
	}
	_ = sink
}

// BenchmarkPoolVsArenaThroughput compares Pool vs Arena throughput.
func BenchmarkPoolVsArenaThroughput(b *testing.B) {
	pool, err := NewPool(AllocatorConfig{
		PoolSize:  64 * 1024 * 1024,
		SlabSize:  1 * 1024 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	})
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}

	arena, err := NewArena(8 * 1024 * 1024)
	if err != nil {
		b.Fatalf("NewArena failed: %v", err)
	}
	defer arena.Free()

	b.Run("Pool.Allocate", func(b *testing.B) {
		benchmarkAllocBatch(b, pool, 64, 1000)
	})

	b.Run("Arena.Alloc", func(b *testing.B) {
		const allocSize = 64
		const batchSize = 1000

		b.ReportAllocs()
		b.SetBytes(allocSize)

		var sink byte
		b.ResetTimer()

		for i := 0; i < b.N; i += batchSize {
			batch := batchSize
			if i+batch > b.N {
				batch = b.N - i
			}
			arena.Reset()
			for j := 0; j < batch; j++ {
				ptr, err := arena.Alloc(allocSize)
				if err != nil {
					b.Fatal(err)
				}
				sink = *(*byte)(ptr)
			}
		}
		_ = sink
	})

	pool.Reset()
}

// BenchmarkPoolPreallocVsLazy compares pre-allocated vs lazy slab creation.
func BenchmarkPoolPreallocVsLazy(b *testing.B) {
	b.Run("Prealloc=true", func(b *testing.B) {
		cfg := AllocatorConfig{
			PoolSize:  64 * 1024 * 1024,
			SlabSize:  256 * 1024,
			SlabCount: 16,
			Prealloc:  true,
		}
		pool, err := NewPool(cfg)
		if err != nil {
			b.Fatalf("NewPool failed: %v", err)
		}
		benchmarkAllocBatch(b, pool, 64, 500)
		pool.Reset()
	})

	b.Run("Prealloc=false", func(b *testing.B) {
		cfg := AllocatorConfig{
			PoolSize:  64 * 1024 * 1024,
			SlabSize:  256 * 1024,
			SlabCount: 16,
			Prealloc:  false,
		}
		pool, err := NewPool(cfg)
		if err != nil {
			b.Fatalf("NewPool failed: %v", err)
		}
		benchmarkAllocBatch(b, pool, 64, 500)
		pool.Reset()
	})
}

// BenchmarkLargeAllocation measures allocation larger than slab size.
func BenchmarkLargeAllocation(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  512 * 1024 * 1024, // 512MB pool for 1MB allocs
		SlabSize:  256 * 1024,
		SlabCount: 4,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	benchmarkAllocBatch(b, pool, 1024*1024, 100) // 100 × 1MB = 100MB per batch
}

// BenchmarkHintWillNeed measures madvise(MADV_WILLNEED) cost.
func BenchmarkHintWillNeed(b *testing.B) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4 * 1024 * 1024)
	if err != nil {
		b.Fatalf("Allocate failed: %v", err)
	}

	ptr := unsafe.Pointer(&data[0])
	size := len(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Hint(HintWillNeed, ptr, size)
	}
}

// BenchmarkHintDontNeed measures madvise(MADV_DONTNEED/MADV_FREE) cost.
func BenchmarkHintDontNeed(b *testing.B) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4 * 1024 * 1024)
	if err != nil {
		b.Fatalf("Allocate failed: %v", err)
	}

	ptr := unsafe.Pointer(&data[0])
	size := len(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Hint(HintDontNeed, ptr, size)
	}
}

// BenchmarkConcurrentAlloc measures concurrent allocation throughput.
// Uses per-goroutine pools to avoid concurrent Reset + Allocate (undefined behavior).
func BenchmarkConcurrentAlloc(b *testing.B) {
	const allocSize = 256
	const batchSize = 100

	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine gets its own pool — no shared Reset, no races
		cfg := AllocatorConfig{
			PoolSize:  256 * 1024 * 1024,
			SlabSize:  256 * 1024,
			SlabCount: 8,
			Prealloc:  true,
		}
		pool, err := NewPool(cfg)
		if err != nil {
			b.Fatalf("NewPool failed: %v", err)
		}
		defer pool.Reset()

		var sink byte
		allocCount := 0
		for pb.Next() {
			data, err := pool.Allocate(allocSize)
			if err != nil {
				b.Fatalf("Allocate failed: %v", err)
			}
			sink = data[0]
			allocCount++
			// Reset periodically within the goroutine's own pool
			if allocCount%batchSize == 0 {
				pool.Reset()
			}
		}
		_ = sink
	})
}

// BenchmarkConcurrentAllocShared measures concurrent allocation from a single pool
// using WaitGroup-based batch coordination. Reset is only called from the outer
// goroutine between timed batches, guaranteeing quiescence.
func BenchmarkConcurrentAllocShared(b *testing.B) {
	const allocSize = 256
	const batchSize = 100
	const numGoroutines = 8

	cfg := AllocatorConfig{
		PoolSize:  512 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 32,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	var sink byte
	b.ReportAllocs()
	b.SetBytes(allocSize)

	b.ResetTimer()

	for i := 0; i < b.N; i += batchSize {
		batch := batchSize
		if i+batch > b.N {
			batch = b.N - i
		}

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for g := 0; g < numGoroutines; g++ {
			go func() {
				defer wg.Done()
				for j := 0; j < batch/numGoroutines; j++ {
					data, err := pool.Allocate(allocSize)
					if err != nil {
						// Pool exhaustion under concurrent load is non-fatal
						return
					}
					sink = data[0]
				}
			}()
		}

		// Quiescent point: all goroutines complete before Reset
		wg.Wait()
		pool.Reset()
	}
	_ = sink
}

// BenchmarkZeroMemory measures memclr cost.
func BenchmarkZeroMemory(b *testing.B) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	data, err := pool.Allocate(4 * 1024 * 1024)
	if err != nil {
		b.Fatalf("Allocate failed: %v", err)
	}

	ptr := unsafe.Pointer(&data[0])
	size := len(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ZeroMemory(ptr, uintptr(size))
	}
}

// BenchmarkStatsRead measures Pool.Stats() read cost.
func BenchmarkStatsRead(b *testing.B) {
	pool, err := NewPool(AllocatorConfig{
		PoolSize:  64 * 1024 * 1024,
		SlabSize:  1 * 1024 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	})
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	// Allocate to have stats to read
	for i := 0; i < 100; i++ {
		pool.Allocate(1024)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pool.Stats()
	}
}

// BenchmarkSmallAllocVariedSizes exercises different allocation sizes.
func BenchmarkSmallAllocVariedSizes(b *testing.B) {
	cfg := AllocatorConfig{
		PoolSize:  128 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	}
	pool, err := NewPool(cfg)
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	sizes := []uint64{16, 32, 64, 128, 256, 512, 1024, 2048, 4096}
	// Worst case: 1000 × 4096 = 4MB per batch, well within 128MB pool
	const resetEvery = 1000

	b.ReportAllocs()
	var sink byte
	b.ResetTimer()

	j := 0
	for i := 0; i < b.N; i++ {
		size := sizes[j%len(sizes)]
		data, err := pool.Allocate(size)
		if err != nil {
			b.Fatalf("Allocate failed: %v (i=%d, size=%d)", err, i, size)
		}
		sink = data[0]
		j++

		if i%resetEvery == 0 && i > 0 {
			pool.Reset()
		}
	}
	_ = sink
}

// BenchmarkGoHeapUsed measures Go runtime heap usage during Pool allocations.
func BenchmarkGoHeapUsed(b *testing.B) {
	pool, err := NewPool(AllocatorConfig{
		PoolSize:  64 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	})
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Reset()

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			data, err := pool.Allocate(64)
			if err != nil {
				b.Fatalf("Allocate failed: %v", err)
			}
			_ = data[0]
		}
		pool.Reset()
	}

	runtime.ReadMemStats(&m1)
	b.ReportMetric(float64(int64(m1.Alloc)-int64(m0.Alloc))/1024, "heap-delta-KB")
}

// BenchmarkBatchSize measures throughput at different reset batch sizes.
func BenchmarkBatchSize(b *testing.B) {
	batchSizes := []int{10, 100, 1000, 10000}
	for _, bs := range batchSizes {
		b.Run(fmt.Sprintf("Batch=%d", bs), func(b *testing.B) {
			cfg := AllocatorConfig{
				PoolSize:  256 * 1024 * 1024,
				SlabSize:  256 * 1024,
				SlabCount: 16,
				Prealloc:  true,
			}
			pool, _ := NewPool(cfg)
			benchmarkAllocBatch(b, pool, 64, bs)
			pool.Reset()
		})
	}
}
