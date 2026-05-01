// Vector storage: demonstrates storing large embedding vectors off-heap
// to eliminate GC scanning of gigabyte-scale float32 arrays.
//
//	go run ./examples/vector-storage/
package main

import (
	"fmt"
	"math"

	"github.com/xDarkicex/memory"
)

const (
	dim    = 1536 // OpenAI embedding dimension
	vecLen = dim * 4
)

func main() {
	pool, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  128 * 1024 * 1024, // 128MB pool
		SlabSize:  1 * 1024 * 1024,   // 1MB slabs
		SlabCount: 16,
		Prealloc:  true,
	})
	if err != nil {
		panic(err)
	}

	const numVectors = 1000
	vectors := make([][]float32, numVectors)

	// Raw (unsafe) approach: allocate bytes, cast to []float32.
	//     data, _ := pool.Allocate(vecLen)
	//     vec := unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), dim)

	// Typed helper approach: PoolSlice eliminates the unsafe cast.
	for i := 0; i < numVectors; i++ {
		vec, err := memory.PoolSlice[float32](pool, dim)
		if err != nil {
			panic(err)
		}
		vec = vec[:dim] // set len=dim for direct indexing
		for j := 0; j < dim; j++ {
			vec[j] = float32(i+j) * 0.001
		}
		vectors[i] = vec
	}

	// Query vector: same pattern, but on the heap for comparison.
	query := make([]float32, dim)
	for j := 0; j < dim; j++ {
		query[j] = float32(j) * 0.001
	}

	bestIdx, bestSim := 0, float32(-1)
	for i := 0; i < numVectors; i++ {
		sim := cosineSimilarity(query, vectors[i])
		if sim > bestSim {
			bestSim = sim
			bestIdx = i
		}
	}

	s := pool.Stats()
	fmt.Printf("Stored %d vectors (%d dimensions each)\n", numVectors, dim)
	fmt.Printf("Total allocated: %.2f MB\n", float64(s.Allocated)/(1024*1024))
	fmt.Printf("Total reserved:  %.2f MB\n", float64(s.Reserved)/(1024*1024))
	fmt.Printf("Best match: vector[%d] (cosine similarity: %.4f)\n", bestIdx, bestSim)
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
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
