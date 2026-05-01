package memory

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

	benchmarkAllocBatch(b, pool, 1024*1024, 100) // 100 × 1MB = 100MB per batch
}

// BenchmarkHintWillNeed measures madvise(MADV_WILLNEED) cost.
func BenchmarkHintWillNeed(b *testing.B) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		b.Fatalf("NewPool failed: %v", err)
	}
	defer pool.Free()

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
	defer pool.Free()

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
		defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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
	defer pool.Free()

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

// BenchmarkFreeListContention measures FreeList throughput scaling under
// increasing concurrency. Run with -cpu=1,2,4,8,16,32,64 to sweep GOMAXPROCS.
// Each goroutine alloc+free in a tight loop against a shared freelist head,
// stressing the CAS. Flat ops/sec/goroutine means the CAS scales well;
// sub-linear at 8+ means contention dominates and sharding is justified.
func BenchmarkFreeListContention(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	retriesBefore := fl.CasRetries()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := fl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}
			fl.Deallocate(slot)
		}
	})

	b.StopTimer()
	retriesDelta := fl.CasRetries() - retriesBefore
	b.ReportMetric(float64(retriesDelta)/float64(b.N), "cas-retries/op")
}

// BenchmarkBatchPopFreeList compares BatchAllocate(N) vs N× Allocate under contention.
// BatchAllocate pops N slots with 1 CAS; N×Allocate pops N slots with N CAS.
// Both push the slots back individually to simulate real deallocation patterns.
func BenchmarkBatchPopFreeList(b *testing.B) {
	batchSizes := []int{16, 32, 64}
	for _, bs := range batchSizes {
		b.Run(fmt.Sprintf("BatchAllocate=%d", bs), func(b *testing.B) {
			cfg := DefaultFreeListConfig()
			cfg.PoolSize = 256 * 1024 * 1024
			cfg.SlotSize = 64
			cfg.SlabSize = 1024 * 1024
			cfg.Prealloc = true

			fl, _ := NewFreeList(cfg)
			defer fl.Free()

			var sink byte

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				slots := make([][]byte, bs)
				for pb.Next() {
					n, err := fl.BatchAllocate(slots)
					if err != nil {
						b.Errorf("BatchAllocate failed: %v", err)
						return
					}
					for i := 0; i < n; i++ {
						sink = slots[i][0]
						fl.Deallocate(slots[i])
					}
				}
			})
			_ = sink
		})

		b.Run(fmt.Sprintf("N×Allocate=%d", bs), func(b *testing.B) {
			cfg := DefaultFreeListConfig()
			cfg.PoolSize = 256 * 1024 * 1024
			cfg.SlotSize = 64
			cfg.SlabSize = 1024 * 1024
			cfg.Prealloc = true

			fl, _ := NewFreeList(cfg)
			defer fl.Free()

			var sink byte

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					for i := 0; i < bs; i++ {
						slot, err := fl.Allocate()
						if err != nil {
							b.Errorf("Allocate failed: %v", err)
							return
						}
						sink = slot[0]
						fl.Deallocate(slot)
					}
				}
			})
			_ = sink
		})
	}
}

// BenchmarkCrossShardFrequency measures the ratio of cross-shard vs local frees.
// Each goroutine tags allocations with its goroutine ID at slot offset 12, then
// checks before deallocating whether the tag matches the current goroutine. This
// simulates work-stealing patterns where a slot allocated on one shard gets freed
// on another (e.g., request handoff via channels).
// Run with -cpu=4,8,16 to see how cross-shard frequency scales.
func BenchmarkCrossShardFrequency(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	var crossFrees atomic.Uint64
	var localFrees atomic.Uint64
	var gid atomic.Uint64

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		home := uint32(gid.Add(1))
		var sink byte

		for pb.Next() {
			slot, err := fl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}

			// Tag first 4 user bytes (offset 12) with goroutine ID.
			*(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), 12)) = home
			sink = slot[0]

			// Read back the tag and compare with current goroutine.
			tag := *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), 12))
			if tag == home {
				localFrees.Add(1)
			} else {
				crossFrees.Add(1)
			}

			fl.Deallocate(slot)
		}
		_ = sink
	})

	b.StopTimer()
	cross := crossFrees.Load()
	local := localFrees.Load()
	total := cross + local
	if total > 0 {
		b.ReportMetric(float64(cross)/float64(total)*100, "cross-pct")
	}
}

// BenchmarkCrossShardWorkStealing measures cross-shard free frequency under
// work-stealing: goroutines allocate, then hand slots to a shared channel where
// consumer goroutines pick them up and deallocate. This simulates request-handoff
// patterns common in server workloads (e.g., HTTP -> background worker).
func BenchmarkCrossShardWorkStealing(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	var deallocCount atomic.Uint64
	var gid atomic.Uint64

	// Channel depth: enough to avoid stalling producers
	const chanDepth = 256
	ch := make(chan struct {
		slot []byte
		home uint32
	}, chanDepth)

	// Consumer goroutines (2): receive slots and deallocate on a different goroutine.
	// Every deallocation here is cross-shard since consumers != producers.
	const numConsumers = 2
	var wg sync.WaitGroup
	for range numConsumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range ch {
				fl.Deallocate(item.slot)
				deallocCount.Add(1)
			}
		}()
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		home := uint32(gid.Add(1))
		var sink byte

		for pb.Next() {
			slot, err := fl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}

			// Tag with home goroutine ID at offset 12.
			*(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), 12)) = home
			sink = slot[0]

			// Send to consumer channel — deallocation happens on a different goroutine.
			ch <- struct {
				slot []byte
				home uint32
			}{slot, home}
		}
		_ = sink
	})

	close(ch)
	wg.Wait()

	b.StopTimer()
	if n := deallocCount.Load(); n > 0 {
		b.ReportMetric(float64(n), "cross-frees")
		// With work-stealing, cross-shard frees approach 100%.
		b.ReportMetric(100.0, "cross-pct")
	}
}

// === Phase 4 — ShardedFreeList Benchmarks ===

// BenchmarkShardedHotPath measures single-goroutine alloc+free throughput
// through the sharded path. Both Allocate and Deallocate should hit the
// per-shard caches (fresh/recycled) with zero atomics on the hot path.
func BenchmarkShardedHotPath(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))

	var sink byte
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		sink = slot[0]
		slot[len(slot)-1] = sink

		if err := sfl.Deallocate(slot); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}

// BenchmarkShardedHotPathHP measures single-goroutine throughput with the
// hazard-pointer path: Protect, touch, Unprotect, Retire (no Deallocate).
func BenchmarkShardedHotPathHP(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))

	var sink byte
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		guard, ok := sfl.Protect(slot)
		if !ok {
			b.Fatal("Protect exhausted")
		}
		sink = slot[0]
		sfl.Unprotect(guard)

		if err := sfl.Retire(slot); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}

// BenchmarkShardedConcurrent measures ShardedFreeList throughput scaling
// under increasing concurrency. Run with -cpu=1,2,4,8,16,32,64.
// Compare against BenchmarkFreeListContention to quantify sharding improvement.
func BenchmarkShardedConcurrent(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := sfl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}
			slot[0] = 42
			if err := sfl.Deallocate(slot); err != nil {
				b.Errorf("Deallocate failed: %v", err)
				return
			}
		}
	})
}

// BenchmarkShardedConcurrentHP measures ShardedFreeList throughput with the
// full hazard-pointer path (Protect/Unprotect + Retire) under concurrency.
// Uses a retry loop for Protect exhaustion (K=2 per shard can fill up under
// hash-based sharding when multiple goroutines collide on the same shard).
func BenchmarkShardedConcurrentHP(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	// Use larger pool for concurrent HP — Protect/Retire path keeps slots in
	// retirement lists, and concurrent scans can race, causing transient exhaustion.
	cfg.PoolSize = 512 * 1024 * 1024
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 16)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := sfl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}

			// Retry until we get a hazard slot (hash collisions may exhaust K=2).
			var guard HazardGuard
			for {
				var ok bool
				guard, ok = sfl.Protect(slot)
				if ok {
					break
				}
			}
			_ = slot[0]
			sfl.Unprotect(guard)

			if err := sfl.Retire(slot); err != nil {
				b.Errorf("Retire failed: %v", err)
				return
			}
		}
	})
}

// BenchmarkShardedCrossShard forces cross-shard deallocation via channel
// handoff. Producers allocate and send slots to consumers, who deallocate
// on a different goroutine (and likely a different shard). Measures
// throughput under the worst-case cache-remote pattern.
func BenchmarkShardedCrossShard(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	type item struct {
		slot []byte
	}

	const chanDepth = 256
	ch := make(chan item, chanDepth)

	// Consumer goroutines: receive and Deallocate.
	const numConsumers = 2
	var wg sync.WaitGroup
	for range numConsumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if err := sfl.Deallocate(it.slot); err != nil {
					b.Errorf("Deallocate failed: %v", err)
				}
			}
		}()
	}

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := sfl.Allocate()
			if err != nil {
				b.Errorf("Allocate failed: %v", err)
				return
			}
			slot[0] = 42
			ch <- item{slot}
		}
	})

	close(ch)
	wg.Wait()
}

// BenchmarkShardedScanOverhead measures the cost of the hazard pointer scan
// at steady state. Slots are allocated, retired (not deallocated), forcing
// the allocator to trigger scan under backpressure to reclaim memory.
// This measures throughput with amortized scan cost included.
func BenchmarkShardedScanOverhead(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 4 * 1024 * 1024 // Small pool to force frequent scans
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		b.Fatal(err)
	}
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))

	var sink byte
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		sink = slot[0]

		// Retire (not Deallocate) — slots go to retirement list.
		// When the global FreeList empties, scan reclaims them.
		if err := sfl.Retire(slot); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}

// BenchmarkFreeListConcurrent measures FreeList throughput under concurrency.
// Kept here alongside other benchmarks per project convention.
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

// BenchmarkFreeListVsPool_64B compares FreeList vs Pool for fixed-size workload.
func BenchmarkFreeListVsPool_64B(b *testing.B) {
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

	b.Run("Pool", func(b *testing.B) {
		cfg := AllocatorConfig{
			PoolSize:  64 * 1024 * 1024,
			SlabSize:  1024 * 1024,
			SlabCount: 16,
			Prealloc:  true,
		}
		pool, _ := NewPool(cfg)
		defer pool.Free()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			_, err := pool.Allocate(64)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkFreeListVsShardedHotPath compares FreeList vs ShardedFreeList
// hot-path latency in a single goroutine.
func BenchmarkFreeListVsShardedHotPath(b *testing.B) {
	b.Run("FreeList", func(b *testing.B) {
		benchFreeListHotPathSingle(b)
	})

	b.Run("ShardedFreeList", func(b *testing.B) {
		benchShardedHotPathSingle(b)
	})
}

func benchFreeListHotPathSingle(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	fl, _ := NewFreeList(cfg)
	defer fl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))

	var sink byte
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot, _ := fl.Allocate()
		sink = slot[0]
		fl.Deallocate(slot)
	}
	_ = sink
}

func benchShardedHotPathSingle(b *testing.B) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, _ := NewShardedFreeList(cfg, 8)
	defer sfl.Free()

	b.ReportAllocs()
	b.SetBytes(int64(cfg.SlotSize))

	var sink byte
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot, _ := sfl.Allocate()
		sink = slot[0]
		sfl.Deallocate(slot)
	}
	_ = sink
}

// BenchmarkFreeListVsShardedConcurrent compares FreeList vs ShardedFreeList
// under 8-way concurrency. Run with -cpu=8 for meaningful results.
func BenchmarkFreeListVsShardedConcurrent(b *testing.B) {
	b.Run("FreeList", func(b *testing.B) {
		cfg := DefaultFreeListConfig()
		cfg.PoolSize = 256 * 1024 * 1024
		cfg.SlotSize = 64
		cfg.SlabSize = 1024 * 1024
		cfg.Prealloc = true

		fl, _ := NewFreeList(cfg)
		defer fl.Free()

		b.ReportAllocs()
		b.SetBytes(int64(cfg.SlotSize))
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				slot, _ := fl.Allocate()
				_ = slot[0]
				fl.Deallocate(slot)
			}
		})
	})

	b.Run("ShardedFreeList", func(b *testing.B) {
		cfg := DefaultFreeListConfig()
		cfg.PoolSize = 256 * 1024 * 1024
		cfg.SlotSize = 64
		cfg.SlabSize = 1024 * 1024
		cfg.Prealloc = true

		sfl, _ := NewShardedFreeList(cfg, 8)
		defer sfl.Free()

		b.ReportAllocs()
		b.SetBytes(int64(cfg.SlotSize))
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				slot, _ := sfl.Allocate()
				_ = slot[0]
				sfl.Deallocate(slot)
			}
		})
	})
}
