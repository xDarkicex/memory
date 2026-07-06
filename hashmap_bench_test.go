package memory

import (
	"runtime"
	"math/rand"
	"sync"
	"testing"
)

// BenchmarkHashMap_PutGet_Concurrent rigorously tests the MESI 128-byte alignment
// under heavy multi-threaded contention (Apple Silicon / AVX-512 targeted).
func BenchmarkHashMap_PutGet_Concurrent(b *testing.B) {
	cfg := HashMapConfig{
		Capacity:  1000, // Small capacity forces aggressive concurrent resizing!
		Alignment: 128,
	}
	m, err := NewHashMap(cfg)
	if err != nil {
		b.Fatalf("Failed to initialize HashMap: %v", err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Thread-local pseudo-random sequence for extreme throughput testing
		rng := rand.Uint64()
		arena, _ := NewArena(4096, 8)
		defer arena.Free()
		val, _ := arena.Alloc(8)
		GlobalDummy = val

		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27

			// 50% reads, 50% writes
			if rng&1 == 0 {
				m.Put(rng, val)
			} else {
				m.Get(rng)
			}
		}
	})
}

// BenchmarkHashMap_Put_Sequential tests pure insertion throughput
func BenchmarkHashMap_Put_Sequential(b *testing.B) {
	cfg := HashMapConfig{
		Capacity:  uint64(b.N) * 2, // Ensure no resizing for raw baseline
		Alignment: 128,
	}
	m, err := NewHashMap(cfg)
	if err != nil {
		b.Fatalf("Failed to initialize HashMap: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	val, _ := arena.Alloc(8)
	GlobalDummy = val

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Put(uint64(i), val)
	}
	runtime.KeepAlive(val)
}

// BenchmarkHashMap_Get_Sequential tests pure query throughput
func BenchmarkHashMap_Get_Sequential(b *testing.B) {
	const benchGetKeys = uint64(1 << 16)

	cfg := HashMapConfig{
		Capacity:  benchGetKeys * 2,
		Alignment: 128,
	}
	m, err := NewHashMap(cfg)
	if err != nil {
		b.Fatalf("Failed to initialize HashMap: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	val, _ := arena.Alloc(8)
	GlobalDummy = val
	for i := uint64(0); i < benchGetKeys; i++ {
		m.Put(uint64(i), val)
	}
	runtime.KeepAlive(val)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Get(uint64(i) & (benchGetKeys - 1))
	}
}

// BenchmarkHashMap_SyncMap_Comparison provides a baseline comparison against the standard library
func BenchmarkHashMap_SyncMap_Comparison(b *testing.B) {
	var m sync.Map

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27

			if rng&1 == 0 {
				m.Store(rng, 1)
			} else {
				m.Load(rng)
			}
		}
	})
}
