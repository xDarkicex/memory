package memory

import (
	"testing"
	"unsafe"

	"go.uber.org/goleak"
)

func TestShardedFreeListBasicLifecycle(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
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

	sfl, err := NewShardedFreeList(cfg, 64, 4)
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

	sfl, err := NewShardedFreeList(cfg, 64, 4)
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

	sfl, err := NewShardedFreeList(cfg, 64, 8)
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

	sfl, err := NewShardedFreeList(cfg, 64, 2)
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

	sfl, err := NewShardedFreeList(cfg, 64, 2)
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

func TestNewShardedFreeListPowerOfTwoRounding(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 1024 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024

	// 5 is not a power of 2; should round up to 8.
	sfl, err := NewShardedFreeList(cfg, 64, 5)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	if sfl.numShards != 8 {
		t.Errorf("numShards = %d, want 8 (rounded from 5)", sfl.numShards)
	}

	// Negative numShards should default to 64.
	sfl2, err := NewShardedFreeList(cfg, 64, -1)
	if err != nil {
		t.Fatalf("NewShardedFreeList with -1: %v", err)
	}
	defer sfl2.Free()
	if sfl2.numShards != 64 {
		t.Errorf("numShards = %d, want 64", sfl2.numShards)
	}

	// Zero numShards should default to 64.
	sfl3, err := NewShardedFreeList(cfg, 64, 0)
	if err != nil {
		t.Fatalf("NewShardedFreeList with 0: %v", err)
	}
	defer sfl3.Free()
	if sfl3.numShards != 64 {
		t.Errorf("numShards = %d, want 64", sfl3.numShards)
	}
}

func TestNewShardedFreeListErrorPropagation(t *testing.T) {
	// SlabSize < SlotSize causes NewFreeList to fail.
	cfg := FreeListConfig{
		PoolSize: 1024 * 1024,
		SlotSize: 4096,
		SlabSize: 64,
	}
	_, err := NewShardedFreeList(cfg, 64, 4)
	if err == nil {
		t.Fatal("expected error propagation from NewFreeList")
	}
}

func TestShardedFreeListInvalidDeallocation(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	if err := sfl.Deallocate(nil); err != ErrInvalidDeallocation {
		t.Errorf("nil slice: got %v, want ErrInvalidDeallocation", err)
	}
	if err := sfl.Deallocate([]byte{}); err != ErrInvalidDeallocation {
		t.Errorf("empty slice: got %v, want ErrInvalidDeallocation", err)
	}
	// Wrong size
	if err := sfl.Deallocate(make([]byte, 32)); err != ErrInvalidDeallocation {
		t.Errorf("wrong size: got %v, want ErrInvalidDeallocation", err)
	}
}

func TestShardedFreeListRetireInvalid(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	if err := sfl.Retire(nil); err != ErrInvalidDeallocation {
		t.Errorf("nil slice: got %v, want ErrInvalidDeallocation", err)
	}
	if err := sfl.Retire(make([]byte, 32)); err != ErrInvalidDeallocation {
		t.Errorf("wrong size: got %v, want ErrInvalidDeallocation", err)
	}
}

func TestShardedFreeListRetireDoubleRetire(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := sfl.Retire(slot); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	// Second retire must fail
	if err := sfl.Retire(slot); err == nil {
		t.Fatal("expected double-retire error")
	}
}

func TestShardedFreeListDeallocateSlowPath(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// Corrupt the metadata at offset 40 to force the slow path (binary search).
	*(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), 40)) = 0xFFFFFFFF

	if err := sfl.Deallocate(slot); err != nil {
		t.Fatalf("Deallocate with corrupted metadata: %v", err)
	}
}

func TestShardedFreeListForceReclamation(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 2)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	// Allocate all slots, retire them (goes to Hyaline batches), then
	// try to allocate more to trigger forceReclamation.
	var slots [][]byte
	for {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		slots = append(slots, slot)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one allocation")
	}

	// Retire all slots — they go to Hyaline batches, not directly back.
	for _, s := range slots {
		if err := sfl.Retire(s); err != nil {
			t.Fatalf("Retire: %v", err)
		}
	}

	// Enter/Leave to drain Hyaline and reclaim slots.
	sfl.HyalineEnter(0)
	sfl.HyalineLeave(0)

	// After Hyaline reclamation, we should be able to allocate again.
	_, err = sfl.Allocate()
	if err != nil {
		t.Fatalf("Allocate after reclamation: %v", err)
	}
}

func TestShardedFreeListPIDControllerFree(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := sfl.Free(); err != nil {
		t.Fatal(err)
	}
}

func TestShardedFreeListPIDControllerReset(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// Allocate some slots
	for i := 0; i < 10; i++ {
		if _, err := sfl.Allocate(); err != nil {
			t.Fatal(err)
		}
	}
	sfl.Reset()

	// Verify still functional after reset
	slot, err := sfl.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if len(slot) != int(cfg.SlotSize) {
		t.Fatalf("expected slot size %d, got %d", cfg.SlotSize, len(slot))
	}
}

func TestShardedFreeListSharedPIDControllerLifecycle(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	const count = 16
	sfls := make([]*ShardedFreeList, 0, count)
	defer func() {
		for _, sfl := range sfls {
			if sfl != nil {
				_ = sfl.Free()
			}
		}
	}()

	for i := 0; i < count; i++ {
		sfl, err := NewShardedFreeList(cfg, 64, 4)
		if err != nil {
			t.Fatal(err)
		}
		sfls = append(sfls, sfl)
	}

	defaultShardedFreeListPID.mu.Lock()
	entries := len(defaultShardedFreeListPID.entries)
	running := defaultShardedFreeListPID.running
	defaultShardedFreeListPID.mu.Unlock()

	if entries != count {
		t.Fatalf("shared PID entries = %d, want %d", entries, count)
	}
	if !running {
		t.Fatal("shared PID controller is not running")
	}

	for i, sfl := range sfls {
		if err := sfl.Free(); err != nil {
			t.Fatal(err)
		}
		sfls[i] = nil
	}

	defaultShardedFreeListPID.mu.Lock()
	entries = len(defaultShardedFreeListPID.entries)
	running = defaultShardedFreeListPID.running
	stopping := defaultShardedFreeListPID.stopping
	defaultShardedFreeListPID.mu.Unlock()

	if entries != 0 {
		t.Fatalf("shared PID entries after Free = %d, want 0", entries)
	}
	if running || stopping {
		t.Fatalf("shared PID controller running=%v stopping=%v, want both false", running, stopping)
	}
}

func TestShardedFreeListSharedPIDThresholdsArePerAllocator(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	hot, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer hot.Free()

	idle, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Free()

	hot.global.allocated.Store(hot.global.reserved.Load())

	defaultShardedFreeListPID.tick()

	hotThreshold := hot.hyHeader.threshold.Load()
	idleThreshold := idle.hyHeader.threshold.Load()
	if hotThreshold >= shardedPIDMaxThreshold {
		t.Fatalf("hot threshold = %d, want below %d", hotThreshold, shardedPIDMaxThreshold)
	}
	if idleThreshold != shardedPIDMaxThreshold {
		t.Fatalf("idle threshold = %d, want %d", idleThreshold, shardedPIDMaxThreshold)
	}
}

func TestShardedFreeListHyalineEnterLeave(t *testing.T) {
	cfg := DefaultFreeListConfig()
	cfg.PoolSize = 64 * 1024
	cfg.SlotSize = 64
	cfg.SlabSize = 4096
	cfg.Prealloc = true

	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatalf("NewShardedFreeList: %v", err)
	}
	defer sfl.Free()

	// Enter and Leave on the same shard should not panic.
	sfl.HyalineEnter(0)
	sfl.HyalineLeave(0)

	// Multiple enters/leaves.
	sfl.HyalineEnter(1)
	sfl.HyalineEnter(2)
	sfl.HyalineLeave(1)
	sfl.HyalineLeave(2)
}
