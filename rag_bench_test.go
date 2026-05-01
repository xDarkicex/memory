// RAG workload benchmarks: simulate vector index build + cosine search + concurrent
// queries. Compares off-heap Pool, Slabby, and standard Go heap allocation.
//
//	go test -bench=RAG -benchmem -count=3 ./...

package memory_test

import (
	"math"
	"sync"
	"testing"
	"unsafe"

	"github.com/xDarkicex/memory"
	"github.com/xDarkicex/slabby"
)

const (
	ragDim       = 1536 // OpenAI embedding dimension
	ragSlotSize  = ragDim * 4
	ragIndexSize = 10_000
)

// --- Shared helpers ---

func newRAGPool(tb testing.TB) *memory.Pool {
	tb.Helper()
	p, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  256 * 1024 * 1024,
		SlabSize:  2 * 1024 * 1024,
		SlabCount: 32,
		Prealloc:  true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { p.Free() })
	return p
}

func newRAGSlabby(tb testing.TB) *slabby.Slabby {
	tb.Helper()
	sl, err := slabby.New(ragSlotSize, ragIndexSize, slabby.WithHeapFallback())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { sl.Close() })
	return sl
}

func cosineSim(a, b []float32) float32 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

func topK(query []float32, vectors [][]float32, k int) ([]int, []float32) {
	type pair struct {
		idx int
		sim float32
	}
	best := make([]pair, 0, k)
	for i, v := range vectors {
		sim := cosineSim(query, v)
		if len(best) < k {
			best = append(best, pair{i, sim})
			continue
		}
		worst := 0
		for j := 1; j < k; j++ {
			if best[j].sim < best[worst].sim {
				worst = j
			}
		}
		if sim > best[worst].sim {
			best[worst] = pair{i, sim}
		}
	}
	idxs := make([]int, k)
	scores := make([]float32, k)
	for i, p := range best {
		idxs[i] = p.idx
		scores[i] = p.sim
	}
	return idxs, scores
}

// PoolSlice returns len=0, cap=dim. Reslice to full capacity before use.
func allocVector(pool *memory.Pool) ([]float32, error) {
	vec, err := memory.PoolSlice[float32](pool, ragDim)
	if err != nil {
		return nil, err
	}
	return vec[:ragDim], nil
}

func mustAllocVector(tb testing.TB, pool *memory.Pool) []float32 {
	vec, err := allocVector(pool)
	if err != nil {
		tb.Fatal(err)
	}
	return vec
}

func allocVectorSlabby(sl *slabby.Slabby) ([]float32, error) {
	ref, err := sl.Allocate()
	if err != nil {
		return nil, err
	}
	data := ref.GetBytes()
	return unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), ragDim), nil
}

func mustAllocVectorSlabby(tb testing.TB, sl *slabby.Slabby) []float32 {
	vec, err := allocVectorSlabby(sl)
	if err != nil {
		tb.Fatal(err)
	}
	return vec
}

// --- Index build ---

func BenchmarkRAG_BuildIndex_Pool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		pool := newRAGPool(b)
		for i := 0; i < ragIndexSize; i++ {
			vec, _ := allocVector(pool)
			for j := 0; j < ragDim; j++ {
				vec[j] = float32(i+j) * 0.0001
			}
		}
		pool.Free()
	}
}

func BenchmarkRAG_BuildIndex_Make(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		vectors := make([][]float32, ragIndexSize)
		for i := 0; i < ragIndexSize; i++ {
			vectors[i] = make([]float32, ragDim)
			for j := 0; j < ragDim; j++ {
				vectors[i][j] = float32(i+j) * 0.0001
			}
		}
	}
}

func BenchmarkRAG_BuildIndex_Slabby(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sl := newRAGSlabby(b)
		for i := 0; i < ragIndexSize; i++ {
			vec, _ := allocVectorSlabby(sl)
			for j := 0; j < ragDim; j++ {
				vec[j] = float32(i+j) * 0.0001
			}
		}
		sl.Close()
	}
}

// --- Single query (top-10 cosine search over 10K vectors) ---

func BenchmarkRAG_Query_Pool(b *testing.B) {
	pool := newRAGPool(b)
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vec := mustAllocVector(b, pool)
		for j := 0; j < ragDim; j++ {
			vec[j] = float32(i+j) * 0.0001
		}
		vectors[i] = vec
	}
	query := vectors[ragIndexSize/2]

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		topK(query, vectors, 10)
	}
}

func BenchmarkRAG_Query_Make(b *testing.B) {
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}
	query := vectors[ragIndexSize/2]

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		topK(query, vectors, 10)
	}
}

func BenchmarkRAG_Query_Slabby(b *testing.B) {
	sl := newRAGSlabby(b)
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vec := mustAllocVectorSlabby(b, sl)
		for j := 0; j < ragDim; j++ {
			vec[j] = float32(i+j) * 0.0001
		}
		vectors[i] = vec
	}
	query := vectors[ragIndexSize/2]

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		topK(query, vectors, 10)
	}
}

// --- Concurrent query (goroutines = GOMAXPROCS, each searches full index) ---

func BenchmarkRAG_ConcurrentQuery_Pool(b *testing.B) {
	pool := newRAGPool(b)
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vec := mustAllocVector(b, pool)
		for j := 0; j < ragDim; j++ {
			vec[j] = float32(i+j) * 0.0001
		}
		vectors[i] = vec
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		query := make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			query[j] = float32(j) * 0.001
		}
		for pb.Next() {
			topK(query, vectors, 10)
		}
	})
}

func BenchmarkRAG_ConcurrentQuery_Make(b *testing.B) {
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		query := make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			query[j] = float32(j) * 0.001
		}
		for pb.Next() {
			topK(query, vectors, 10)
		}
	})
}

func BenchmarkRAG_ConcurrentQuery_Slabby(b *testing.B) {
	sl := newRAGSlabby(b)
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vec := mustAllocVectorSlabby(b, sl)
		for j := 0; j < ragDim; j++ {
			vec[j] = float32(i+j) * 0.0001
		}
		vectors[i] = vec
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		query := make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			query[j] = float32(j) * 0.001
		}
		for pb.Next() {
			topK(query, vectors, 10)
		}
	})
}

// --- Request-scoped: allocate scratch buffer, encode, search, reset ---

func BenchmarkRAG_RequestLifecycle_Pool(b *testing.B) {
	pool := newRAGPool(b)
	// Vectors are the persistent index — allocate on Go heap so Reset()
	// only reclaims scratch buffers, not the index.
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf, _ := memory.PoolSlice[byte](pool, 4096)
		_ = buf
		query := vectors[b.N%ragIndexSize]
		topK(query, vectors, 10)
		pool.Reset()
	}
}

func BenchmarkRAG_RequestLifecycle_Make(b *testing.B) {
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf := make([]byte, 4096)
		_ = buf
		query := vectors[b.N%ragIndexSize]
		topK(query, vectors, 10)
	}
}

// --- Concurrent request lifecycle (multi-goroutine request handling) ---

func BenchmarkRAG_ConcurrentRequestLifecycle_Pool(b *testing.B) {
	pool := newRAGPool(b)
	// Vectors are the persistent index — allocate on Go heap so concurrent
	// scratch allocations don't exhaust the pool.
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			buf, _ := memory.PoolSlice[byte](pool, 4096)
			_ = buf
			query := vectors[i%ragIndexSize]
			topK(query, vectors, 10)
			i++
		}
	})
}

func BenchmarkRAG_ConcurrentRequestLifecycle_Make(b *testing.B) {
	vectors := make([][]float32, ragIndexSize)
	for i := 0; i < ragIndexSize; i++ {
		vectors[i] = make([]float32, ragDim)
		for j := 0; j < ragDim; j++ {
			vectors[i][j] = float32(i+j) * 0.0001
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			buf := make([]byte, 4096)
			_ = buf
			query := vectors[i%ragIndexSize]
			topK(query, vectors, 10)
			i++
		}
	})
}

// --- Per-vector allocation throughput ---

// BenchmarkRAG_PerVector_Alloc_Pool measures the cost of a single vector
// allocation from Pool (hot path, CAS-based slab alloc). The pool is
// sized to hold all iterations without Reset so we measure pure allocation
// cost, not mmap syscall overhead.
func BenchmarkRAG_PerVector_Alloc_Pool(b *testing.B) {
	pool, err := memory.NewPool(memory.AllocatorConfig{
		// 1 TB virtual pool size to ensure b.Loop() never exhausts the pool.
		// Since Prealloc is false, this only allocates a few MBs of metadata slices.
		PoolSize:  1024 * 1024 * 1024 * 1024,
		SlabSize:  2 * 1024 * 1024,
		SlabCount: 1,
		Prealloc:  false,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { pool.Free() })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		vec, err := allocVector(pool)
		if err != nil {
			b.Fatal(err)
		}
		vec[0] = 1.0
	}
}

func BenchmarkRAG_PerVector_Alloc_Make(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var sink []float32
	for b.Loop() {
		sink = make([]float32, ragDim)
		sink[0] = 1.0
	}
	_ = sink
}

func BenchmarkRAG_PerVector_Alloc_Slabby(b *testing.B) {
	sl := newRAGSlabby(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ref := sl.MustAllocate()
		data := ref.GetBytes()
		vec := unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), ragDim)
		vec[0] = 1.0
		sl.Deallocate(ref)
	}
}

// --- Concurrent index build ---

func BenchmarkRAG_ConcurrentBuild_Pool(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		pool := newRAGPool(b)
		var wg sync.WaitGroup
		perG := ragIndexSize / 8
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perG; i++ {
					vec, _ := allocVector(pool)
					vec[0] = float32(i)
				}
			}()
		}
		wg.Wait()
		pool.Free()
	}
}

func BenchmarkRAG_ConcurrentBuild_Make(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var mu sync.Mutex
		vectors := make([][]float32, 0, ragIndexSize)
		var wg sync.WaitGroup
		perG := ragIndexSize / 8
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perG; i++ {
					vec := make([]float32, ragDim)
					vec[0] = float32(i)
					mu.Lock()
					vectors = append(vectors, vec)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
}
