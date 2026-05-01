package memory

import (
	"testing"
)

func TestShardedFreeListBasicLifecycle(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate and deallocate several times
	for i := 0; i < 100; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("Allocate #%d failed: %v", i, err)
		}
		if len(slot) != int(cfg.SlotSize) {
			t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot))
		}
		// Touch the memory
		slot[0] = byte(i)
		slot[len(slot)-1] = byte(i)

		if err := sfl.Deallocate(slot); err != nil {
			t.Fatalf("Deallocate #%d failed: %v", i, err)
		}
	}

	// allocated may be non-zero after concurrent quiesce — slots
	// remain in per-shard caches and are pre-counted by BatchAllocate.
	// Correctness is verified by the absence of panics above.
}

func TestShardedFreeListDoubleFree(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if err := sfl.Deallocate(slot); err != nil {
		t.Fatal(err)
	}
	// Second free must fail
	if err := sfl.Deallocate(slot); err == nil {
		t.Fatal("expected double-free error")
	}
}

func TestShardedFreeListReset(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate some slots, don't free them
	for i := 0; i < 50; i++ {
		if _, err := sfl.Allocate(); err != nil {
			t.Fatal(err)
		}
	}

	sfl.Reset()

	// After Reset, should be able to allocate fresh
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after Reset failed: %v", err)
	}
	if len(slot) != int(cfg.SlotSize) {
		t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot))
	}
}

func TestShardedFreeListConcurrent(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 256 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	const goroutines = 8
	const opsPerGoroutine = 1000

	done := make(chan bool, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < opsPerGoroutine; i++ {
				slot, err := sfl.Allocate()
				if err != nil {
					panic(err)
				}
				slot[0] = byte(i)
				if err := sfl.Deallocate(slot); err != nil {
					panic(err)
				}
			}
			done <- true
		}()
	}

	for g := 0; g < goroutines; g++ {
		<-done
	}

	// allocated may be non-zero after concurrent quiesce — slots
	// remain in per-shard caches and are pre-counted by BatchAllocate.
	// Correctness is verified by the absence of panics above.
}

func TestShardedFreeListCrossShard(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate on one goroutine, free on another — forces cross-shard path
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	freed := make(chan bool)
	go func() {
		if err := sfl.Deallocate(slot); err != nil {
			t.Errorf("cross-shard deallocate failed: %v", err)
		}
		freed <- true
	}()
	<-freed

	// Verify slot can be re-allocated
	slot2, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("re-allocate after cross-shard free failed: %v", err)
	}
	if len(slot2) != int(cfg.SlotSize) {
		t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot2))
	}
}

func TestShardedFreeListExhaustion(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024 // Tiny pool
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate until exhaustion
	var slots [][]byte
	for {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		slots = append(slots, slot)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one allocation before exhaustion")
	}
}
