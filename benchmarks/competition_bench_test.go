// Competition benchmarks: memory allocators vs slabby vs raw make.
//
// Throughput (ns/op) via standard Go benchmarks.
// Latency p50/p99 via fixed-iteration collection + sort.
//
// All comparisons use the same slot/object sizes and total capacities
// for a fair head-to-head.
//
//	go test -bench=Competition -benchmem -count=5 ./...
package memory_test

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/xDarkicex/memory"
	"github.com/xDarkicex/slabby"
)

// ---------------------------------------------------------------------------
// Shared configuration
// ---------------------------------------------------------------------------

const (
	compSlotSize  = 72 // sizeof(CompRecord)=56 + metaOffset=12 = 68, rounded up
	compSlabSize  = 64 * 1024 // 64KB
	compSlabCount = 64        // enough for many iterations without exhaustion
	compPoolSize  = 64 * 1024 * 1024 // 64MB
	compNumShards = 8
)

// ---------------------------------------------------------------------------
// Type used for typed-allocation comparisons
// ---------------------------------------------------------------------------

type CompRecord struct {
	ID      uint64
	Payload [48]byte
}

// ---------------------------------------------------------------------------
// Setup helpers
// ---------------------------------------------------------------------------

func newCompFreeList(tb testing.TB) *memory.FreeList {
	tb.Helper()
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = compSlotSize
	cfg.SlabSize = compSlabSize
	cfg.SlabCount = compSlabCount
	cfg.PoolSize = compPoolSize
	cfg.Prealloc = true
	fl, err := memory.NewFreeList(cfg, 64)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { fl.Free() })
	return fl
}

func newCompShardedFreeList(tb testing.TB) *memory.ShardedFreeList {
	tb.Helper()
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = compSlotSize
	cfg.SlabSize = compSlabSize
	cfg.SlabCount = compSlabCount
	cfg.PoolSize = compPoolSize
	cfg.Prealloc = true
	sfl, err := memory.NewShardedFreeList(cfg, 64, compNumShards)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sfl.Free() })
	return sfl
}

func newCompSlabby(tb testing.TB) *slabby.Slabby {
	tb.Helper()
	sl, err := slabby.New(compSlotSize, compSlabCount*compSlabSize/compSlotSize,
		slabby.WithHeapFallback(),
	)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sl.Close() })
	return sl
}

func newCompPool(tb testing.TB) *memory.Pool {
	tb.Helper()
	pool, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  compPoolSize,
		SlabCount: compPoolSize / compSlabSize,
		SlabSize:  compSlabSize,
		Prealloc:  true,
	}, 64)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { pool.Free() })
	return pool
}

func newCompArena(tb testing.TB) *memory.Arena {
	tb.Helper()
	arena, err := memory.NewArena(compPoolSize, 64)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { arena.Free() })
	return arena
}

// ---------------------------------------------------------------------------
// 1. Fixed-size allocation throughput (single goroutine)
// ---------------------------------------------------------------------------

func BenchmarkCompetition_Alloc_FreeList(b *testing.B) {
	fl := newCompFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		slot, _ := fl.Allocate()
		fl.Deallocate(slot)
	}
}

func BenchmarkCompetition_Alloc_ShardedFreeList(b *testing.B) {
	sfl := newCompShardedFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		slot, _ := sfl.Allocate()
		sfl.Deallocate(slot)
	}
}

func BenchmarkCompetition_Alloc_Slabby(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ref := sl.MustAllocate()
		sl.Deallocate(ref)
	}
}

func BenchmarkCompetition_Alloc_SlabbyFast(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, id, _ := sl.AllocateFast()
		sl.DeallocateFast(id)
	}
}

func BenchmarkCompetition_Alloc_Make(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		s := make([]byte, compSlotSize)
		_ = s
	}
}

// ---------------------------------------------------------------------------
// 2. Fixed-size concurrent throughput
// ---------------------------------------------------------------------------

func BenchmarkCompetition_Concurrent_FreeList(b *testing.B) {
	fl := newCompFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, _ := fl.Allocate()
			fl.Deallocate(slot)
		}
	})
}

func BenchmarkCompetition_Concurrent_ShardedFreeList(b *testing.B) {
	sfl := newCompShardedFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, _ := sfl.Allocate()
			sfl.Deallocate(slot)
		}
	})
}

func BenchmarkCompetition_Concurrent_Slabby(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ref := sl.MustAllocate()
			sl.Deallocate(ref)
		}
	})
}

func BenchmarkCompetition_Concurrent_SlabbyFast(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, id, _ := sl.AllocateFast()
			sl.DeallocateFast(id)
		}
	})
}

// ---------------------------------------------------------------------------
// 3. Typed allocation throughput (single goroutine)
// ---------------------------------------------------------------------------

func BenchmarkCompetition_Typed_FreeListAlloc(b *testing.B) {
	fl := newCompFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		rec, err := memory.FreeListAlloc[CompRecord](fl)
		if err != nil {
			b.Fatal(err)
		}
		memory.FreeListDealloc(fl, rec)
	}
}

func BenchmarkCompetition_Typed_SlabbyUnsafe(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ref := sl.MustAllocate()
		data := ref.GetBytes()
		rec := (*CompRecord)(unsafe.Pointer(&data[0]))
		_ = rec
		sl.Deallocate(ref)
	}
}

func BenchmarkCompetition_Typed_MakeStruct(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		rec := &CompRecord{}
		_ = rec
	}
}

// ---------------------------------------------------------------------------
// 4. Variable-size allocation throughput
// ---------------------------------------------------------------------------

func BenchmarkCompetition_VarAlloc_Pool(b *testing.B) {
	pool := newCompPool(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		buf, _ := pool.Allocate(compSlotSize)
		_ = buf
		pool.Reset()
	}
}

func BenchmarkCompetition_VarAlloc_Arena(b *testing.B) {
	arena := newCompArena(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = arena.Alloc(compSlotSize)
		arena.Reset()
	}
}

func BenchmarkCompetition_VarAlloc_Slabby(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ref := sl.MustAllocate()
		sl.Deallocate(ref)
	}
}

func BenchmarkCompetition_VarAlloc_Make(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		s := make([]byte, compSlotSize)
		_ = s
	}
}

// ---------------------------------------------------------------------------
// 5. Latency percentile measurement
//
// Each benchmark runs N iterations, collects per-op durations, and reports
// p50 and p99 as custom metrics. Timer overhead is amortized: each iteration
// does 100 alloc+free cycles and divides.
// ---------------------------------------------------------------------------

const latencyIterations = 100_000
const latencyBatchSize = 100

// measureLatency runs a batch of alloc/free operations and returns per-op duration.
// Batching amortizes time.Now() overhead for sub-microsecond operations.
func measureLatency(fn func()) time.Duration {
	start := time.Now()
	for i := 0; i < latencyBatchSize; i++ {
		fn()
	}
	return time.Since(start) / latencyBatchSize
}

// reportPercentiles sorts durations and reports p50, p99 as custom metrics.
func reportPercentiles(b *testing.B, durations []time.Duration) {
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[len(durations)/2]
	p99 := durations[len(durations)*99/100]
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns")
}

func BenchmarkCompetition_Latency_FreeList(b *testing.B) {
	fl := newCompFreeList(b)
	durations := make([]time.Duration, latencyIterations)

	for i := 0; i < latencyIterations; i++ {
		durations[i] = measureLatency(func() {
			slot, _ := fl.Allocate()
			fl.Deallocate(slot)
		})
	}
	reportPercentiles(b, durations)
}

func BenchmarkCompetition_Latency_ShardedFreeList(b *testing.B) {
	sfl := newCompShardedFreeList(b)
	durations := make([]time.Duration, latencyIterations)

	for i := 0; i < latencyIterations; i++ {
		durations[i] = measureLatency(func() {
			slot, _ := sfl.Allocate()
			sfl.Deallocate(slot)
		})
	}
	reportPercentiles(b, durations)
}

func BenchmarkCompetition_Latency_Slabby(b *testing.B) {
	sl := newCompSlabby(b)
	durations := make([]time.Duration, latencyIterations)

	for i := 0; i < latencyIterations; i++ {
		durations[i] = measureLatency(func() {
			ref := sl.MustAllocate()
			sl.Deallocate(ref)
		})
	}
	reportPercentiles(b, durations)
}

func BenchmarkCompetition_Latency_SlabbyFast(b *testing.B) {
	sl := newCompSlabby(b)
	durations := make([]time.Duration, latencyIterations)

	for i := 0; i < latencyIterations; i++ {
		durations[i] = measureLatency(func() {
			_, id, _ := sl.AllocateFast()
			sl.DeallocateFast(id)
		})
	}
	reportPercentiles(b, durations)
}

func BenchmarkCompetition_Latency_Make(b *testing.B) {
	durations := make([]time.Duration, latencyIterations)

	for i := 0; i < latencyIterations; i++ {
		durations[i] = measureLatency(func() {
			_ = make([]byte, compSlotSize)
		})
	}
	reportPercentiles(b, durations)
}

// ---------------------------------------------------------------------------
// 6. Concurrent latency (simulated: N goroutines, each does M ops, merged)
// ---------------------------------------------------------------------------

func concurrentLatency(b *testing.B, numGoroutines int, fn func()) {
	durations := make([]time.Duration, latencyIterations)
	opsPerG := latencyIterations / numGoroutines

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < opsPerG; i++ {
				durations[offset+i] = measureLatency(fn)
			}
		}(g * opsPerG)
	}
	wg.Wait()
	reportPercentiles(b, durations)
}

func BenchmarkCompetition_ConcLatency_FreeList(b *testing.B) {
	fl := newCompFreeList(b)
	concurrentLatency(b, 8, func() {
		slot, _ := fl.Allocate()
		fl.Deallocate(slot)
	})
}

func BenchmarkCompetition_ConcLatency_ShardedFreeList(b *testing.B) {
	sfl := newCompShardedFreeList(b)
	concurrentLatency(b, 8, func() {
		slot, _ := sfl.Allocate()
		sfl.Deallocate(slot)
	})
}

func BenchmarkCompetition_ConcLatency_Slabby(b *testing.B) {
	sl := newCompSlabby(b)
	concurrentLatency(b, 8, func() {
		ref := sl.MustAllocate()
		sl.Deallocate(ref)
	})
}

func BenchmarkCompetition_ConcLatency_SlabbyFast(b *testing.B) {
	sl := newCompSlabby(b)
	concurrentLatency(b, 8, func() {
		_, id, _ := sl.AllocateFast()
		sl.DeallocateFast(id)
	})
}

// ---------------------------------------------------------------------------
// 7. Bulk allocation throughput
// ---------------------------------------------------------------------------

func BenchmarkCompetition_Bulk_FreeList_BatchAllocate(b *testing.B) {
	fl := newCompFreeList(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var slots [32][]byte
		n, _ := fl.BatchAllocate(slots[:])
		for i := 0; i < n; i++ {
			fl.Deallocate(slots[i])
		}
	}
}

func BenchmarkCompetition_Bulk_Slabby_BatchAllocate(b *testing.B) {
	sl := newCompSlabby(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		refs, _ := sl.BatchAllocate(32)
		sl.BatchDeallocate(refs)
	}
}

// ---------------------------------------------------------------------------
// 8. Summary helper — generates a comparison table
// ---------------------------------------------------------------------------

func TestCompetitionSummary(t *testing.T) {
	fmt.Println(`
╔══════════════════════════════════════════════════════════════╗
║  COMPETITION BENCHMARKS                                     ║
║  Run: go test -bench=Competition -benchmem -count=5 ./...   ║
║                                                              ║
║  Covers:                                                     ║
║    Alloc      — fixed-size alloc+dealloc throughput          ║
║    Concurrent — parallel alloc+dealloc (GOMAXPROCS goroutines)║
║    Typed      — typed allocator comparison (FreeListAlloc)   ║
║    VarAlloc   — variable-size allocator throughput           ║
║    Latency    — p50/p99 latency percentiles                  ║
║    ConcLatency— p50/p99 under concurrency (8 goroutines)     ║
║    Bulk       — batch allocate/deallocate throughput         ║
╚══════════════════════════════════════════════════════════════╝`)
}

// ---------------------------------------------------------------------------
// 9. BFS Buffer benchmarks — Get/Put pattern, fixed-size large buffers
// ---------------------------------------------------------------------------

const (
	bfsBitsetSize    = 131072
	bfsFrontierSize  = 65536
	bfsBitsetSlots   = 8
	bfsFrontierSlots = 8
)

func newBFSBitsetSlabby(tb testing.TB) *slabby.Slabby {
	tb.Helper()
	sl, err := slabby.New(bfsBitsetSize, bfsBitsetSlots)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sl.Close() })
	return sl
}

func newBFSFrontierSlabby(tb testing.TB) *slabby.Slabby {
	tb.Helper()
	sl, err := slabby.New(bfsFrontierSize, bfsFrontierSlots)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sl.Close() })
	return sl
}

func newBFSBitsetMemory(tb testing.TB) *memory.ShardedFreeList {
	tb.Helper()
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = bfsBitsetSize
	cfg.SlabSize = 2 * 1024 * 1024
	cfg.SlabCount = bfsBitsetSlots
	cfg.PoolSize = uint64(bfsBitsetSlots+2) * cfg.SlabSize
	cfg.Prealloc = true
	sfl, err := memory.NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sfl.Free() })
	return sfl
}

func newBFSFrontierMemory(tb testing.TB) *memory.ShardedFreeList {
	tb.Helper()
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = bfsFrontierSize
	cfg.SlabSize = 1024 * 1024
	cfg.SlabCount = bfsFrontierSlots
	cfg.PoolSize = uint64(bfsFrontierSlots+2) * cfg.SlabSize
	cfg.Prealloc = true
	sfl, err := memory.NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sfl.Free() })
	return sfl
}

func BenchmarkBFSBuffer_GetPut_Sequential(b *testing.B) {
	b.Run("slabby", func(b *testing.B) {
		bitset := newBFSBitsetSlabby(b)
		frontier := newBFSFrontierSlabby(b)

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			ref1 := bitset.MustAllocate()
			data1 := ref1.GetBytes()
			data1[0] = 1

			ref2 := frontier.MustAllocate()
			data2 := ref2.GetBytes()
			data2[0] = 1

			bitset.Deallocate(ref1)
			frontier.Deallocate(ref2)
		}
	})

	b.Run("memory", func(b *testing.B) {
		bitset := newBFSBitsetMemory(b)
		frontier := newBFSFrontierMemory(b)

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			slot1, _ := bitset.Allocate()
			slot2, _ := frontier.Allocate()
			slot1[0] = 1
			slot2[0] = 1
			bitset.Deallocate(slot1)
			frontier.Deallocate(slot2)
		}
	})
}

func BenchmarkBFSBuffer_GetPut_Concurrent(b *testing.B) {
	b.Run("slabby", func(b *testing.B) {
		// Capacity must be large enough for GOMAXPROCS concurrent goroutines
		// each holding 2 buffers (bitset + frontier) simultaneously.
		concurrentSlots := 128
		bitset, _ := slabby.New(bfsBitsetSize, concurrentSlots, slabby.WithHeapFallback())
		frontier, _ := slabby.New(bfsFrontierSize, concurrentSlots, slabby.WithHeapFallback())
		defer bitset.Close()
		defer frontier.Close()

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				ref1 := bitset.MustAllocate()
				data1 := ref1.GetBytes()
				data1[0] = byte(i)

				ref2 := frontier.MustAllocate()
				data2 := ref2.GetBytes()
				data2[0] = byte(i)

				bitset.Deallocate(ref1)
				frontier.Deallocate(ref2)
				i++
			}
		})
	})

	b.Run("memory", func(b *testing.B) {
		bitset := newBFSBitsetMemory(b)
		frontier := newBFSFrontierMemory(b)

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				slot1, _ := bitset.Allocate()
				slot2, _ := frontier.Allocate()
				slot1[0] = byte(i)
				slot2[0] = byte(i)
				bitset.Deallocate(slot1)
				frontier.Deallocate(slot2)
				i++
			}
		})
	})
}

// ---------------------------------------------------------------------------
// 10. Edge bulk allocation benchmarks
// ---------------------------------------------------------------------------

const (
	edgeSlotSize   = 80
	edgeAllocCount = 1000000
)

func BenchmarkEdgeBulkAlloc_1M(b *testing.B) {
	b.Run("slabby", func(b *testing.B) {
		sl, err := slabby.New(edgeSlotSize, edgeAllocCount,
			slabby.WithHeapFallback(),
		)
		if err != nil {
			b.Fatal(err)
		}
		defer sl.Close()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			refs := make([]*slabby.SlabRef, edgeAllocCount)
			for i := range refs {
				refs[i] = sl.MustAllocate()
				data := refs[i].GetBytes()
				data[0] = byte(i)
			}
			for _, ref := range refs {
				sl.Deallocate(ref)
			}
		}
	})

	b.Run("memory", func(b *testing.B) {
		cfg := memory.DefaultFreeListConfig()
		cfg.SlotSize = edgeSlotSize
		cfg.SlabSize = 2 * 1024 * 1024
		cfg.SlabCount = 64
		cfg.PoolSize = uint64(edgeAllocCount)*edgeSlotSize + 16*1024*1024
		cfg.Prealloc = true
		sfl, err := memory.NewShardedFreeList(cfg, 64, 64)
		if err != nil {
			b.Fatal(err)
		}
		defer sfl.Free()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			slots := make([][]byte, edgeAllocCount)
			for i := range slots {
				slot, err := sfl.Allocate()
				if err != nil {
					b.Fatal(err)
				}
				slot[0] = byte(i)
				slots[i] = slot
			}
			for _, slot := range slots {
				sfl.Deallocate(slot)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 11. Edge table page bulk benchmarks (4KB pages)
// ---------------------------------------------------------------------------

const (
	pageSlotSize   = 4096
	pageAllocCount = 128 * 1024
)

func BenchmarkEdgeTablePageBulk_128K(b *testing.B) {
	b.Run("slabby", func(b *testing.B) {
		sl, err := slabby.New(pageSlotSize, pageAllocCount,
			slabby.WithHeapFallback(),
		)
		if err != nil {
			b.Fatal(err)
		}
		defer sl.Close()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			refs := make([]*slabby.SlabRef, pageAllocCount)
			for i := range refs {
				refs[i] = sl.MustAllocate()
				data := refs[i].GetBytes()
				data[0] = byte(i)
			}
			for _, ref := range refs {
				sl.Deallocate(ref)
			}
		}
	})

	b.Run("memory", func(b *testing.B) {
		cfg := memory.DefaultFreeListConfig()
		cfg.SlotSize = pageSlotSize
		cfg.SlabSize = 16 * 1024 * 1024
		cfg.SlabCount = 32
		cfg.PoolSize = uint64(pageAllocCount)*pageSlotSize + 64*1024*1024
		cfg.Prealloc = true
		sfl, err := memory.NewShardedFreeList(cfg, 64, 64)
		if err != nil {
			b.Fatal(err)
		}
		defer sfl.Free()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			slots := make([][]byte, pageAllocCount)
			for i := range slots {
				slot, err := sfl.Allocate()
				if err != nil {
					b.Fatal(err)
				}
				slot[0] = byte(i)
				slots[i] = slot
			}
			for _, slot := range slots {
				sfl.Deallocate(slot)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 12. Concurrent AddEdge — 8 workers, graph hot path
// ---------------------------------------------------------------------------

const (
	concurrentEdgeWorkers     = 8
	concurrentEdgesPerWorker  = 125000
)

func BenchmarkConcurrentAddEdge_8Workers(b *testing.B) {
	b.Run("slabby", func(b *testing.B) {
		sl, err := slabby.New(edgeSlotSize, concurrentEdgeWorkers*concurrentEdgesPerWorker,
			slabby.WithHeapFallback(),
		)
		if err != nil {
			b.Fatal(err)
		}
		defer sl.Close()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			var wg sync.WaitGroup
			wg.Add(concurrentEdgeWorkers)
			allRefs := make([][]*slabby.SlabRef, concurrentEdgeWorkers)
			for w := 0; w < concurrentEdgeWorkers; w++ {
				go func(workerID int) {
					defer wg.Done()
					refs := make([]*slabby.SlabRef, concurrentEdgesPerWorker)
					for i := range refs {
						refs[i] = sl.MustAllocate()
						data := refs[i].GetBytes()
						data[0] = byte(workerID)
						data[1] = byte(i)
					}
					allRefs[workerID] = refs
				}(w)
			}
			wg.Wait()
			for _, refs := range allRefs {
				for _, ref := range refs {
					sl.Deallocate(ref)
				}
			}
		}
	})

	b.Run("memory", func(b *testing.B) {
		cfg := memory.DefaultFreeListConfig()
		cfg.SlotSize = edgeSlotSize
		cfg.SlabSize = 2 * 1024 * 1024
		cfg.SlabCount = 64
		cfg.PoolSize = uint64(concurrentEdgeWorkers*concurrentEdgesPerWorker)*edgeSlotSize + 32*1024*1024
		cfg.Prealloc = true
		sfl, err := memory.NewShardedFreeList(cfg, 64, 64)
		if err != nil {
			b.Fatal(err)
		}
		defer sfl.Free()

		b.ResetTimer()
		b.ReportAllocs()

		for b.Loop() {
			var wg sync.WaitGroup
			wg.Add(concurrentEdgeWorkers)
			allSlots := make([][][]byte, concurrentEdgeWorkers)
			for w := 0; w < concurrentEdgeWorkers; w++ {
				go func(workerID int) {
					defer wg.Done()
					slots := make([][]byte, concurrentEdgesPerWorker)
					for i := range slots {
						slot, err := sfl.Allocate()
						if err != nil {
							panic(err)
						}
						slot[0] = byte(workerID)
						slot[1] = byte(i)
						slots[i] = slot
					}
					allSlots[workerID] = slots
				}(w)
			}
			wg.Wait()
			for _, batch := range allSlots {
				for _, slot := range batch {
					sfl.Deallocate(slot)
				}
			}
		}
	})
}
