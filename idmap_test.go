package memory

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func newIDMapTestValueArena(t *testing.T, count int) (*Arena, []unsafe.Pointer) {
	t.Helper()
	arena, err := NewArena(uint64(max(count, 1))*8, 8)
	if err != nil {
		t.Fatalf("new value arena: %v", err)
	}
	values := make([]unsafe.Pointer, count)
	for i := range values {
		values[i], err = arena.Alloc(8)
		if err != nil {
			arena.Free()
			t.Fatalf("allocate value %d: %v", i, err)
		}
		*(*uint64)(values[i]) = uint64(i + 1)
	}
	return arena, values
}

func TestIDMapBucketIsTwoCacheLines(t *testing.T) {
	if got := unsafe.Sizeof(IDBucket{}); got != idMapBucketBytes {
		t.Fatalf("IDBucket size=%d want=%d", got, idMapBucketBytes)
	}
}

func TestIDMapRejectsUnsupportedAlignment(t *testing.T) {
	if _, err := NewIDMap(IDMapConfig{Capacity: 8, Alignment: 3}); err == nil {
		t.Fatal("non-power-of-two alignment accepted")
	}
	if _, err := NewIDMap(IDMapConfig{Capacity: 8, Alignment: 256}); err == nil {
		t.Fatal("alignment larger than bucket stride accepted")
	}
}

func TestIDMapKeyBoundAndFreeLifecycle(t *testing.T) {
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: 4})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	arena, values := newIDMapTestValueArena(t, 1)
	defer arena.Free()
	if err := m.PutString("identifier-too-large", values[0]); err != ErrArenaExhausted {
		t.Fatalf("key bound error=%v want=%v", err, ErrArenaExhausted)
	}
	if err := m.Free(); err != nil {
		t.Fatalf("free: %v", err)
	}
	if err := m.Free(); err != nil {
		t.Fatalf("second free: %v", err)
	}
	if err := m.PutString("after-free", values[0]); err != ErrIDMapFreed {
		t.Fatalf("put after free error=%v", err)
	}
	if _, ok := m.GetString("after-free"); ok {
		t.Fatal("get after free succeeded")
	}
	if m.DeleteString("after-free") {
		t.Fatal("delete after free succeeded")
	}
}

func TestIDMapFingerprintMaskFindsEveryExactMatch(t *testing.T) {
	for target := uint64(0); target < 256; target++ {
		for slot := uint64(0); slot < idMapSlotsPerBucket; slot++ {
			meta := idMapSlotMask << idMapOccupiedShift
			for lane := uint64(0); lane < idMapSlotsPerBucket; lane++ {
				fingerprint := (target + lane + 1) & 0xff
				if lane == slot {
					fingerprint = target
				}
				meta |= fingerprint << (idMapFingerprintAt + lane*8)
			}
			if got := idMapFingerprintMask(meta, target); got&(1<<slot) == 0 {
				t.Fatalf("target=%d slot=%d mask=%04b", target, slot, got)
			}
		}
	}
}

func TestIDMapStringAndBytes(t *testing.T) {
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: 4096})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, 3)
	defer arena.Free()

	if err := m.PutString("alpha", values[0]); err != nil {
		t.Fatalf("put string: %v", err)
	}
	mutable := []byte("bravo")
	if err := m.PutBytes(mutable, values[1]); err != nil {
		t.Fatalf("put bytes: %v", err)
	}
	copy(mutable, "xxxxx")

	if got, ok := m.GetString("alpha"); !ok || got != values[0] {
		t.Fatalf("get alpha=%p ok=%v", got, ok)
	}
	if got, ok := m.GetString("bravo"); !ok || got != values[1] {
		t.Fatalf("copied byte key get=%p ok=%v", got, ok)
	}
	if err := m.PutString("", values[2]); err != nil {
		t.Fatalf("put empty ID: %v", err)
	}
	if got, ok := m.GetString(""); !ok || got != values[2] {
		t.Fatalf("empty ID get=%p ok=%v", got, ok)
	}
}

func TestIDMapExactHashCollision(t *testing.T) {
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: 4096})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, 2)
	defer arena.Free()

	left, _ := idMapStringKey("collision-left")
	right, _ := idMapStringKey("collision-right")
	right.hash = left.hash
	if err := m.put(left, values[0], false, nil); err != nil {
		t.Fatalf("put left: %v", err)
	}
	if err := m.put(right, values[1], false, nil); err != nil {
		t.Fatalf("put right: %v", err)
	}
	if got, ok := m.get(left); !ok || got != values[0] {
		t.Fatalf("left collision lookup=%p ok=%v", got, ok)
	}
	if got, ok := m.get(right); !ok || got != values[1] {
		t.Fatalf("right collision lookup=%p ok=%v", got, ok)
	}
}

func TestIDMapPutIfAbsentDeleteAndReinsert(t *testing.T) {
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: 4096})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, 3)
	defer arena.Free()

	got, inserted, err := m.PutStringIfAbsent("id", values[0])
	if err != nil || !inserted || got != values[0] {
		t.Fatalf("first put-if-absent got=%p inserted=%v err=%v", got, inserted, err)
	}
	got, inserted, err = m.PutStringIfAbsent("id", values[1])
	if err != nil || inserted || got != values[0] {
		t.Fatalf("second put-if-absent got=%p inserted=%v err=%v", got, inserted, err)
	}
	if !m.DeleteString("id") || m.DeleteString("id") {
		t.Fatal("delete semantics incorrect")
	}
	if err := m.PutString("id", values[2]); err != nil {
		t.Fatalf("reinsert: %v", err)
	}
	if got, ok := m.GetString("id"); !ok || got != values[2] {
		t.Fatalf("reinsert lookup=%p ok=%v", got, ok)
	}
}

func TestIDMapResizePreservesIDs(t *testing.T) {
	const count = 4096
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: count * 32})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, count)
	defer arena.Free()
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("semantic-document-%08d", i)
		if err := m.PutString(ids[i], values[i]); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	for i := range ids {
		if got, ok := m.GetString(ids[i]); !ok || got != values[i] {
			t.Fatalf("get %d=%p ok=%v", i, got, ok)
		}
	}
}

func TestIDMapConcurrentSameAndUniqueIDs(t *testing.T) {
	const workers = 8
	const each = 512
	m, err := NewIDMap(IDMapConfig{Capacity: 64, KeyBytes: workers * each * 32})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, workers*each+workers)
	defer arena.Free()

	ids := make([][]string, workers)
	for worker := range ids {
		ids[worker] = make([]string, each)
		for i := range ids[worker] {
			ids[worker][i] = fmt.Sprintf("worker-%02d-id-%05d", worker, i)
		}
	}

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range ids[worker] {
				if err := m.PutString(id, values[worker*each+i]); err != nil {
					t.Errorf("worker %d put %d: %v", worker, i, err)
					return
				}
				if err := m.PutString("shared-id", values[workers*each+worker]); err != nil {
					t.Errorf("worker %d shared put: %v", worker, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for worker := range ids {
		for i, id := range ids[worker] {
			if got, ok := m.GetString(id); !ok || got != values[worker*each+i] {
				t.Fatalf("worker %d get %d=%p ok=%v", worker, i, got, ok)
			}
		}
	}
	if _, ok := m.GetString("shared-id"); !ok {
		t.Fatal("shared ID missing")
	}
}

func TestIDMapConcurrentPutIfAbsentHasSinglePublisher(t *testing.T) {
	const workers = 32
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: 4096})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, workers)
	defer arena.Free()

	var inserted atomic.Uint32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, won, err := m.PutStringIfAbsent("single-publisher", values[worker])
			if err != nil {
				t.Errorf("worker %d: %v", worker, err)
				return
			}
			if got == nil {
				t.Errorf("worker %d returned nil", worker)
			}
			if won {
				inserted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := inserted.Load(); got != 1 {
		t.Fatalf("publishers=%d want=1", got)
	}
}

func TestIDMapConcurrentResize(t *testing.T) {
	const workers = 8
	const each = 2048
	m, err := NewIDMap(IDMapConfig{Capacity: 8, KeyBytes: workers * each * 24})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, workers*each)
	defer arena.Free()

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id := fmt.Sprintf("resize-%02d-%06d", worker, i)
				if err := m.PutString(id, values[worker*each+i]); err != nil {
					t.Errorf("worker %d put %d: %v", worker, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for worker := 0; worker < workers; worker++ {
		for i := 0; i < each; i++ {
			id := fmt.Sprintf("resize-%02d-%06d", worker, i)
			if got, ok := m.GetString(id); !ok || got != values[worker*each+i] {
				t.Fatalf("worker %d get %d=%p ok=%v", worker, i, got, ok)
			}
		}
	}
}

func TestTypedIDMap(t *testing.T) {
	type value struct{ ordinal uint64 }
	m, err := NewTypedIDMap[value](IDMapConfig{Capacity: 8, KeyBytes: 4096})
	if err != nil {
		t.Fatalf("new typed ID map: %v", err)
	}
	defer m.Free()
	pool, err := NewFreeList(FreeListConfig{PoolSize: 4096, SlotSize: 64, SlabSize: 4096, SlabCount: 1, Prealloc: true}, 64)
	if err != nil {
		t.Fatalf("new value free list: %v", err)
	}
	defer pool.Free()
	stored, err := FreeListAlloc[value](pool)
	if err != nil {
		t.Fatalf("allocate typed value: %v", err)
	}
	stored.ordinal = 42
	if err := m.PutString("typed", stored); err != nil {
		t.Fatalf("typed put: %v", err)
	}
	got, ok := m.GetString("typed")
	if !ok || got.ordinal != 42 {
		t.Fatalf("typed get=%v ok=%v", got, ok)
	}
}

func TestIDMapSteadyStateZeroHeapAllocations(t *testing.T) {
	m, err := NewIDMap(IDMapConfig{Capacity: 1024, KeyBytes: 1 << 20})
	if err != nil {
		t.Fatalf("new ID map: %v", err)
	}
	defer m.Free()
	arena, values := newIDMapTestValueArena(t, 2)
	defer arena.Free()
	if err := m.PutString("allocation-contract", values[0]); err != nil {
		t.Fatalf("initial put: %v", err)
	}

	allocs := testing.AllocsPerRun(1000, func() {
		if err := m.PutString("allocation-contract", values[1]); err != nil {
			panic(err)
		}
		if got, ok := m.GetString("allocation-contract"); !ok || got != values[1] {
			panic("lookup mismatch")
		}
	})
	if allocs != 0 {
		t.Fatalf("steady-state IDMap operations allocated: %.2f", allocs)
	}
	runtime.KeepAlive(values)
}
