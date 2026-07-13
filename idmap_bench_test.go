package memory

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

const idMapBenchmarkKeys = 1 << 16

func benchmarkIDMapFixture(b *testing.B) (*IDMap, *Arena, []string, unsafe.Pointer) {
	b.Helper()
	m, err := NewIDMap(IDMapConfig{Capacity: idMapBenchmarkKeys * 2, KeyBytes: idMapBenchmarkKeys * 32})
	if err != nil {
		b.Fatalf("new ID map: %v", err)
	}
	arena, err := NewArena(4096, 8)
	if err != nil {
		m.Free()
		b.Fatalf("new value arena: %v", err)
	}
	value, err := arena.Alloc(8)
	if err != nil {
		arena.Free()
		m.Free()
		b.Fatalf("allocate value: %v", err)
	}
	ids := make([]string, idMapBenchmarkKeys)
	for i := range ids {
		ids[i] = fmt.Sprintf("document-%016x", i)
	}
	return m, arena, ids, value
}

func BenchmarkIDMapPutString(b *testing.B) {
	m, arena, ids, value := benchmarkIDMapFixture(b)
	defer m.Free()
	defer arena.Free()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.PutString(ids[i&(idMapBenchmarkKeys-1)], value); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIDMapGetString(b *testing.B) {
	m, arena, ids, value := benchmarkIDMapFixture(b)
	defer m.Free()
	defer arena.Free()
	for _, id := range ids {
		if err := m.PutString(id, value); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.GetString(ids[i&(idMapBenchmarkKeys-1)])
	}
}

func BenchmarkGoMapGetString(b *testing.B) {
	unused, arena, ids, value := benchmarkIDMapFixture(b)
	defer unused.Free()
	defer arena.Free()
	m := make(map[string]unsafe.Pointer, len(ids))
	for _, id := range ids {
		m[id] = value
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m[ids[i&(idMapBenchmarkKeys-1)]]
	}
}

func BenchmarkSyncMapGetString(b *testing.B) {
	unused, arena, ids, value := benchmarkIDMapFixture(b)
	defer unused.Free()
	defer arena.Free()
	var m sync.Map
	for _, id := range ids {
		m.Store(id, value)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Load(ids[i&(idMapBenchmarkKeys-1)])
	}
}

func BenchmarkSyncMapPutString(b *testing.B) {
	unused, arena, ids, value := benchmarkIDMapFixture(b)
	defer unused.Free()
	defer arena.Free()
	var m sync.Map
	for _, id := range ids {
		m.Store(id, value)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Store(ids[i&(idMapBenchmarkKeys-1)], value)
	}
}

func BenchmarkIDMapMixedParallel(b *testing.B) {
	m, arena, ids, value := benchmarkIDMapFixture(b)
	defer m.Free()
	defer arena.Free()
	for _, id := range ids {
		if err := m.PutString(id, value); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := uint64(0x9e3779b97f4a7c15)
		for pb.Next() {
			index ^= index >> 12
			index ^= index << 25
			index ^= index >> 27
			id := ids[index&(idMapBenchmarkKeys-1)]
			if index&3 == 0 {
				if err := m.PutString(id, value); err != nil {
					b.Error(err)
					return
				}
			} else {
				m.GetString(id)
			}
		}
	})
}

func BenchmarkSyncMapMixedParallel(b *testing.B) {
	unused, arena, ids, value := benchmarkIDMapFixture(b)
	defer unused.Free()
	defer arena.Free()
	var m sync.Map
	for _, id := range ids {
		m.Store(id, value)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := uint64(0x9e3779b97f4a7c15)
		for pb.Next() {
			index ^= index >> 12
			index ^= index << 25
			index ^= index >> 27
			id := ids[index&(idMapBenchmarkKeys-1)]
			if index&3 == 0 {
				m.Store(id, value)
			} else {
				m.Load(id)
			}
		}
	})
}
