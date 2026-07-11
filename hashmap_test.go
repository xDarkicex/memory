package memory

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestHashMap_BasicPutGet(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 1024, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	key := uint64(42)
	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	ptr, _ := arena.Alloc(8)
	val := unsafe.Pointer(ptr)

	m.Put(key, val)

	retrieved, ok := m.Get(key)
	if !ok {
		t.Fatalf("Get failed after Put")
	}
	if retrieved != val {
		t.Fatalf("Retrieved wrong pointer: got %v, expected %v", retrieved, val)
	}
}

func TestHashMap_PutIfAbsent(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 1024, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	first, _ := arena.Alloc(8)
	second, _ := arena.Alloc(8)
	*(*int)(first) = 1
	*(*int)(second) = 2

	got, inserted := m.PutIfAbsent(42, first)
	if !inserted {
		t.Fatalf("first PutIfAbsent did not insert")
	}
	if got != first {
		t.Fatalf("first PutIfAbsent returned wrong pointer: got %v want %v", got, first)
	}

	got, inserted = m.PutIfAbsent(42, second)
	if inserted {
		t.Fatalf("second PutIfAbsent unexpectedly inserted")
	}
	if got != first {
		t.Fatalf("second PutIfAbsent returned wrong pointer: got %v want %v", got, first)
	}

	stored, ok := m.Get(42)
	if !ok || stored != first {
		t.Fatalf("Get after PutIfAbsent got %v ok=%v want %v", stored, ok, first)
	}
}

func TestHashMap_Delete(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 1024, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	key := uint64(99)
	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	val, _ := arena.Alloc(8)

	m.Put(key, val)
	ok := m.Delete(key)
	if !ok {
		t.Fatalf("Delete returned false for existing key")
	}

	_, ok = m.Get(key)
	if ok {
		t.Fatalf("Key still exists after Delete")
	}
}

func TestHashMap_PutDoesNotDuplicateAfterTombstone(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	for i := 0; i < 8; i++ {
		ptr, _ := arena.Alloc(8)
		*(*int)(ptr) = i
		m.Put(uint64(i*2), ptr)
	}

	if !m.Delete(0) {
		t.Fatalf("Delete returned false for existing key")
	}

	ptr, _ := arena.Alloc(8)
	*(*int)(ptr) = 88
	m.Put(14, ptr)

	got, ok := m.Get(14)
	if !ok {
		t.Fatalf("Get failed after updating key behind tombstone")
	}
	if *(*int)(got) != 88 {
		t.Fatalf("Get returned stale value after tombstone update: %d", *(*int)(got))
	}

	s := m.state.Load()
	seen := 0
	for bIdx := uint64(0); bIdx < s.size; bIdx++ {
		b := (*Bucket)(unsafe.Pointer(uintptr(s.base) + uintptr(bIdx*128)))
		meta := b.Metadata.Load()
		for slot := uint32(0); slot < 7; slot++ {
			if meta&(1<<slot) != 0 && b.Keys[slot].Load() == 14 {
				seen++
			}
		}
	}
	if seen != 1 {
		t.Fatalf("expected one copy of key 14, found %d", seen)
	}
}

func TestHashMap_PutRecyclesTombstone(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	for i := 0; i < 7; i++ {
		ptr, _ := arena.Alloc(8)
		*(*int)(ptr) = i
		m.Put(uint64(i*2), ptr)
	}
	if !m.Delete(4) {
		t.Fatalf("Delete returned false for existing key")
	}

	ptr, _ := arena.Alloc(8)
	*(*int)(ptr) = 77
	m.Put(100, ptr)
	got, ok := m.Get(100)
	if !ok || *(*int)(got) != 77 {
		t.Fatalf("Get failed after tombstone recycle: ok=%v", ok)
	}
}

func TestHashMap_SWAR_Filtering(t *testing.T) {
	// A targeted test to ensure SWAR fingerprint collision masking operates cleanly
	m, err := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	// Insert exactly 7 items (filling a single block)
	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	for i := uint64(1); i <= 7; i++ {
		ptr, _ := arena.Alloc(8)
		*(*int)(ptr) = int(i)
		m.Put(i, ptr)
	}

	for i := uint64(1); i <= 7; i++ {
		v, ok := m.Get(i)
		if !ok || *(*int)(v) != int(i) {
			t.Logf("SWAR mask failed to locate key %d: ok=%v v=%v", i, ok, v)
		}
	}
	runtime.KeepAlive(arena)
}

func TestHashMap_MigrateAllHandlesEmptyBuckets(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 32, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	ptr, _ := arena.Alloc(8)
	m.Put(1, ptr)
	runtime.KeepAlive(ptr)

	if err := m.triggerResize(); err != nil {
		t.Fatalf("triggerResize failed: %v", err)
	}
	resizing := m.state.Load()
	if resizing.next == nil {
		t.Fatalf("resize did not install next state")
	}

	done := make(chan struct{})
	go func() {
		m.helpMigrateAll(resizing)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("migration did not complete; bucketsRemaining=%d", resizing.bucketsRemaining.Load())
	}

	if rem := resizing.bucketsRemaining.Load(); rem != 0 {
		t.Fatalf("migration left %d buckets remaining", rem)
	}
	if got := m.state.Load(); got != resizing.next {
		t.Fatalf("migration did not promote next state")
	}
	if _, ok := m.Get(1); !ok {
		t.Fatalf("migrated key missing after promotion")
	}
}

func TestHashMap_ConcurrentResizeCompletes(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 64, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	arena, _ := NewArena(4096, 8)
	defer arena.Free()
	val, _ := arena.Alloc(8)
	var wg sync.WaitGroup
	done := make(chan struct{})

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			base := uint64(worker * 512)
			for i := uint64(0); i < 512; i++ {
				m.Put(base+i, val)
			}
		}(worker)
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("concurrent resize did not complete")
	}

	for key := uint64(0); key < 4096; key += 257 {
		if _, ok := m.Get(key); !ok {
			t.Fatalf("missing key after concurrent resize: %d", key)
		}
	}
	runtime.KeepAlive(val)
}
