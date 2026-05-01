package main

import (
	"math"
	"testing"
	"unsafe"

	"github.com/xDarkicex/memory"
)

func newVectorPool(tb testing.TB) *memory.Pool {
	tb.Helper()
	p, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  128 * 1024 * 1024,
		SlabSize:  1 * 1024 * 1024,
		SlabCount: 16,
		Prealloc:  true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func TestVectorStorage(t *testing.T) {
	pool := newVectorPool(t)
	defer pool.Free()

	// Raw API: allocate bytes, cast to []float32 via unsafe.
	data, err := pool.Allocate(vecLen)
	if err != nil {
		t.Fatal(err)
	}
	vec := unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), dim)
	vec[0] = 1.0
	vec[dim-1] = 2.0

	if vec[0] != 1.0 || vec[dim-1] != 2.0 {
		t.Fatal("vector values not preserved")
	}
}

func TestVectorStorageWithHelpers(t *testing.T) {
	pool := newVectorPool(t)
	defer pool.Free()

	// Typed helper: PoolSlice[float32] replaces manual unsafe casting.
	vec, err := memory.PoolSlice[float32](pool, dim)
	if err != nil {
		t.Fatal(err)
	}
	vec = vec[:dim]
	vec[0] = 1.0
	vec[dim-1] = 2.0

	if vec[0] != 1.0 || vec[dim-1] != 2.0 {
		t.Fatal("vector values not preserved with helpers")
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if sim := cosineSimilarity(a, b); math.Abs(float64(sim-1.0)) > 1e-6 {
		t.Fatalf("identical vectors should have cosine similarity 1.0, got %.6f", sim)
	}

	c := []float32{0, 1, 0}
	if sim := cosineSimilarity(a, c); math.Abs(float64(sim)) > 1e-6 {
		t.Fatalf("orthogonal vectors should have cosine similarity 0, got %.6f", sim)
	}
}

func TestVectorStorageReset(t *testing.T) {
	pool := newVectorPool(t)

	for i := 0; i < 5; i++ {
		_, _ = pool.Allocate(vecLen)
		pool.Reset()
	}

	s := pool.Stats()
	if s.Reserved != 0 {
		t.Fatalf("expected 0 reserved after reset, got %d", s.Reserved)
	}
}

func BenchmarkArenaVectorStore(b *testing.B) {
	pool := newVectorPool(b)

	b.ReportAllocs()
	b.SetBytes(int64(vecLen))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		data, _ := pool.Allocate(vecLen)
		vec := unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), dim)
		vec[0] = float32(i)
		pool.Reset()
	}
}

var vectorSink []float32

func BenchmarkStdVectorStore(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(vecLen))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		vec := make([]float32, dim)
		vec[0] = float32(i)
		vectorSink = vec
	}
}

func BenchmarkArenaCosineSearch(b *testing.B) {
	pool := newVectorPool(b)
	const N = 1000
	vectors := make([][]float32, N)
	for i := 0; i < N; i++ {
		data, _ := pool.Allocate(vecLen)
		vectors[i] = unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), dim)
	}

	query := vectors[0]

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		bestIdx, bestSim := 0, float32(-1)
		for j := 0; j < N; j++ {
			sim := cosineSimilarity(query, vectors[j])
			if sim > bestSim {
				bestSim = sim
				bestIdx = j
			}
		}
		_ = bestIdx
	}
}

func BenchmarkStdCosineSearch(b *testing.B) {
	const N = 1000
	vectors := make([][]float32, N)
	for i := 0; i < N; i++ {
		vectors[i] = make([]float32, dim)
		for j := 0; j < dim; j++ {
			vectors[i][j] = float32(i+j) * 0.001
		}
	}

	query := vectors[0]

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		bestIdx, bestSim := 0, float32(-1)
		for j := 0; j < N; j++ {
			sim := cosineSimilarity(query, vectors[j])
			if sim > bestSim {
				bestSim = sim
				bestIdx = j
			}
		}
		_ = bestIdx
	}
}
