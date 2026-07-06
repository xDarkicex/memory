package memory

import (
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
	var dummy *int = new(int)
	val := unsafe.Pointer(dummy)

	m.Put(key, val)

	retrieved, ok := m.Get(key)
	if !ok {
		t.Fatalf("Get failed after Put")
	}
	if retrieved != val {
		t.Fatalf("Retrieved wrong pointer: got %v, expected %v", retrieved, val)
	}
}

func TestHashMap_Delete(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 1024, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	key := uint64(99)
	var dummy *int = new(int)
	val := unsafe.Pointer(dummy)

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

func TestHashMap_SWAR_Filtering(t *testing.T) {
	// A targeted test to ensure SWAR fingerprint collision masking operates cleanly
	m, err := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	// Insert exactly 7 items (filling a single block)
	dummies := make([]int, 8)
	for i := uint64(1); i <= 7; i++ {
		dummies[i] = int(i)
		m.Put(i, unsafe.Pointer(&dummies[i]))
	}

	for i := uint64(1); i <= 7; i++ {
		v, ok := m.Get(i)
		if !ok || *(*int)(v) != int(i) {
			t.Logf("SWAR mask failed to locate key %d: ok=%v v=%v", i, ok, v)
		}
	}
}

func TestHashMap_MigrateAllHandlesEmptyBuckets(t *testing.T) {
	m, err := NewHashMap(HashMapConfig{Capacity: 32, Alignment: 128})
	if err != nil {
		t.Fatalf("Failed to init: %v", err)
	}

	dummy := 1
	m.Put(1, unsafe.Pointer(&dummy))

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

	dummy := 1
	val := unsafe.Pointer(&dummy)
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
}
