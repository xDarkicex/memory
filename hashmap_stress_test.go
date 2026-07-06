package memory

import (
	"sync"
	"testing"
	"unsafe"
)

// TestHashMap_Stress_Concurrent tests extreme contention on the Hopscotch buckets
// forcing CPU yielding via Spin() and wait-free Hopscotch displacements.
func TestHashMap_Stress_Concurrent(t *testing.T) {
	cfg := HashMapConfig{
		Capacity:  1000000,
		Alignment: 128,
	}
	m, err := NewHashMap(cfg)
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	var wg sync.WaitGroup
	numRoutines := 64
	numOps := 5000

	dummy := new(int)
	val := unsafe.Pointer(dummy)

	// Concurrent Inserts
	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				key := uint64(offset*numOps + j)
				m.Put(key, val)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent Reads
	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				key := uint64(offset*numOps + j)
				_, ok := m.Get(key)
				if !ok {
					t.Errorf("Missing key during stress read: %d", key)
				}
			}
		}(i)
	}
	wg.Wait()
}
