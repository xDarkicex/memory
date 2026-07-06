package memory_test

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/xDarkicex/memory"
)

type RagVector [ragDim]float32

// --- RAG Document Store Benchmarks ---
// These benchmarks test using HashMap as a vector document store (mapping uint64 IDs to Vectors),
// comparing it against sync.Map and a standard map with an RWMutex.

func BenchmarkRAG_MapBuildIndex_HashMap(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		pool := newRAGPool(b)
		hm, _ := memory.NewTypedMap[RagVector](memory.HashMapConfig{Capacity: ragIndexSize, Alignment: 128})
		for i := uint64(0); i < ragIndexSize; i++ {
			vec, _ := memory.PoolAlloc[RagVector](pool)
			vec[0] = float32(i)
			hm.Put(i, vec)
		}
		pool.Free()
	}
}

func BenchmarkRAG_MapBuildIndex_SyncMap(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var sm sync.Map
		for i := uint64(0); i < ragIndexSize; i++ {
			vec := new(RagVector)
			vec[0] = float32(i)
			sm.Store(i, vec)
		}
	}
}

func BenchmarkRAG_MapBuildIndex_RWMutexMap(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var mu sync.RWMutex
		m := make(map[uint64]*RagVector, ragIndexSize)
		for i := uint64(0); i < ragIndexSize; i++ {
			vec := new(RagVector)
			vec[0] = float32(i)
			mu.Lock()
			m[i] = vec
			mu.Unlock()
		}
	}
}

func BenchmarkRAG_MapConcurrentLookup_HashMap(b *testing.B) {
	pool := newRAGPool(b)
	hm, _ := memory.NewTypedMap[RagVector](memory.HashMapConfig{Capacity: ragIndexSize, Alignment: 128})
	for i := uint64(0); i < ragIndexSize; i++ {
		vec, _ := memory.PoolAlloc[RagVector](pool)
		vec[0] = float32(i)
		hm.Put(i, vec)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % ragIndexSize
			vec, found := hm.Get(idx)
			if found {
				_ = vec[0]
			}
		}
	})
	pool.Free()
}

func BenchmarkRAG_MapConcurrentLookup_SyncMap(b *testing.B) {
	var sm sync.Map
	for i := uint64(0); i < ragIndexSize; i++ {
		vec := new(RagVector)
		vec[0] = float32(i)
		sm.Store(i, vec)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % ragIndexSize
			v, found := sm.Load(idx)
			if found {
				_ = v.(*RagVector)[0]
			}
		}
	})
}

func BenchmarkRAG_MapConcurrentLookup_RWMutexMap(b *testing.B) {
	var mu sync.RWMutex
	m := make(map[uint64]*RagVector, ragIndexSize)
	for i := uint64(0); i < ragIndexSize; i++ {
		vec := new(RagVector)
		vec[0] = float32(i)
		m[i] = vec
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % ragIndexSize
			mu.RLock()
			vec, found := m[idx]
			mu.RUnlock()
			if found {
				_ = vec[0]
			}
		}
	})
}

// --- Mixed Workload (95% Read, 5% Write) Benchmarks ---
// Simulates a RAG environment with concurrent querying alongside continuous index updates/upserts.

func BenchmarkRAG_MapMixedWorkload_HashMap(b *testing.B) {
	pool := newRAGPool(b)
	hm, _ := memory.NewTypedMap[RagVector](memory.HashMapConfig{Capacity: ragIndexSize, Alignment: 128})
	for i := uint64(0); i < ragIndexSize; i++ {
		vec, _ := memory.PoolAlloc[RagVector](pool)
		vec[0] = float32(i)
		hm.Put(i, vec)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		dummyVec, _ := memory.PoolAlloc[RagVector](pool)
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % (ragIndexSize * 2)
			
			// 5% write mutation rate
			if rng%100 < 5 {
				writeIdx := rng
				dummyVec[0] = float32(writeIdx)
				hm.Put(writeIdx, dummyVec)
			} else {
				vec, found := hm.Get(idx)
				if found {
					_ = vec[0]
				}
			}
		}
	})
	pool.Free()
}

func BenchmarkRAG_MapMixedWorkload_SyncMap(b *testing.B) {
	var sm sync.Map
	for i := uint64(0); i < ragIndexSize; i++ {
		vec := new(RagVector)
		vec[0] = float32(i)
		sm.Store(i, vec)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		dummyVec := new(RagVector)
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % (ragIndexSize * 2)
			
			// 5% write mutation rate
			if rng%100 < 5 {
				writeIdx := rng
				dummyVec[0] = float32(writeIdx)
				sm.Store(writeIdx, dummyVec)
			} else {
				v, found := sm.Load(idx)
				if found {
					_ = v.(*RagVector)[0]
				}
			}
		}
	})
}

func BenchmarkRAG_MapMixedWorkload_RWMutexMap(b *testing.B) {
	var mu sync.RWMutex
	m := make(map[uint64]*RagVector, ragIndexSize)
	for i := uint64(0); i < ragIndexSize; i++ {
		vec := new(RagVector)
		vec[0] = float32(i)
		m[i] = vec
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.Uint64()
		dummyVec := new(RagVector)
		for pb.Next() {
			rng ^= rng >> 12
			rng ^= rng << 25
			rng ^= rng >> 27
			idx := rng % (ragIndexSize * 2)
			
			// 5% write mutation rate
			if rng%100 < 5 {
				writeIdx := rng
				dummyVec[0] = float32(writeIdx)
				mu.Lock()
				m[writeIdx] = dummyVec
				mu.Unlock()
			} else {
				mu.RLock()
				vec, found := m[idx]
				mu.RUnlock()
				if found {
					_ = vec[0]
				}
			}
		}
	})
}
