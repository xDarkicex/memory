package memory

import (
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// testSlotSize is a test slot size large enough for Hyaline metadata + payload.
const testSlotSize = 128

// testBase creates an mmap'd region for testing. Uses real mmap so checkptr
// (enabled under -race) does not track the memory — off-heap pointers stored
// as uint64 and loaded back are opaque to Go's pointer validation.
// The region is automatically unmapped via t.Cleanup.
func testBase(tb testing.TB, size int) unsafe.Pointer {
	tb.Helper()
	data, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		tb.Fatalf("mmap test region: %v", err)
	}
	tb.Cleanup(func() { unix.Munmap(data) })
	return unsafe.Pointer(unsafe.SliceData(data))
}

// testNode returns the actual pointer of a "slot" within the test region.
func testNode(base unsafe.Pointer, idx int) unsafe.Pointer {
	return unsafe.Add(base, idx*testSlotSize)
}

// testFreeFn returns a free function that records freed batch heads.
func testFreeFn(freed *[]uint64) func(unsafe.Pointer) {
	return func(batchHead unsafe.Pointer) {
		*freed = append(*freed, uint64(uintptr(batchHead)))
	}
}

func TestHyalineEnterLeave(t *testing.T) {
	var h hyalineHeader
	hyalineHeaderInit(&h)

	// Enter slot 0.
	hyalineEnter(&h, 0)

	// Verify slot 0 is occupied.
	if v := h.slots[0].head.Load(); v != 0x1 {
		t.Fatalf("after enter: slot[0] = %#x, want 0x1", v)
	}

	// Leave slot 0 — no nodes queued, should be clean.
	var freed []uint64
	hyalineLeave(&h, 0, testFreeFn(&freed))

	// Verify slot 0 is cleared.
	if v := h.slots[0].head.Load(); v != 0 {
		t.Fatalf("after leave: slot[0] = %#x, want 0", v)
	}

	if len(freed) != 0 {
		t.Fatalf("expected 0 freed batches, got %d", len(freed))
	}
}

func TestHyalineEnterLeaveDifferentSlots(t *testing.T) {
	var h hyalineHeader
	hyalineHeaderInit(&h)

	// Enter multiple slots.
	hyalineEnter(&h, 0)
	hyalineEnter(&h, 5)
	hyalineEnter(&h, 10)

	if v := h.slots[0].head.Load(); v != 0x1 {
		t.Fatalf("slot[0] = %#x, want 0x1", v)
	}
	if v := h.slots[5].head.Load(); v != 0x1 {
		t.Fatalf("slot[5] = %#x, want 0x1", v)
	}
	if v := h.slots[10].head.Load(); v != 0x1 {
		t.Fatalf("slot[10] = %#x, want 0x1", v)
	}

	// Leave all slots.
	for _, idx := range []int{0, 5, 10} {
		var freed []uint64
		hyalineLeave(&h, idx, testFreeFn(&freed))
		if len(freed) != 0 {
			t.Fatalf("slot[%d]: expected 0 freed, got %d", idx, len(freed))
		}
		if v := h.slots[idx].head.Load(); v != 0 {
			t.Fatalf("slot[%d] after leave = %#x, want 0", idx, v)
		}
	}
}

func TestHyalineSharedSlotEnterLeave(t *testing.T) {
	// Multiple goroutines can share the same slot.
	var h hyalineHeader
	hyalineHeaderInit(&h)

	// Simulate two goroutines entering the same slot.
	hyalineEnter(&h, 0)
	hyalineEnter(&h, 0) // second goroutine — just re-stores 0x1

	if v := h.slots[0].head.Load(); v != 0x1 {
		t.Fatalf("slot[0] = %#x, want 0x1", v)
	}

	// First goroutine leaves — drains any nodes, clears slot.
	var freed []uint64
	hyalineLeave(&h, 0, testFreeFn(&freed))
	if len(freed) != 0 {
		t.Fatalf("first leave: expected 0 freed, got %d", len(freed))
	}

	// Second goroutine leaves — slot is already 0, should be a no-op.
	hyalineLeave(&h, 0, testFreeFn(&freed))
	if len(freed) != 0 {
		t.Fatalf("second leave: expected 0 freed, got %d", len(freed))
	}
}

func TestHyalineRetireImmediateFree(t *testing.T) {
	// If no slots are occupied when flushing, the batch is freed immediately.
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, (hyalineK+10)*testSlotSize)

	var batch hyalineBatch
	hyalineBatchInit(&batch)

	// Add 3 nodes to the batch.
	n0 := testNode(base, 0)
	n1 := testNode(base, 1)
	n2 := testNode(base, 2)

	var freed []uint64
	fn := testFreeFn(&freed)

	hyalineRetire(&h, &batch, n0, fn)
	hyalineRetire(&h, &batch, n1, fn)
	hyalineRetire(&h, &batch, n2, fn)

	// With only 3 nodes and threshold=65, batch shouldn't flush yet.
	if batch.counter != 3 {
		t.Fatalf("batch counter = %d, want 3", batch.counter)
	}

	// Force flush with fewer than threshold nodes.
	hyalineRetireFlush(&h, &batch, fn)

	// No slots were occupied → batch should be freed immediately.
	if len(freed) != 1 {
		t.Fatalf("expected 1 freed batch head, got %d", len(freed))
	}

}

func TestHyalineRetireWithOccupiedSlots(t *testing.T) {
	// Batch should NOT be freed until all occupied slots leave.
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, (hyalineK+10)*testSlotSize)

	// Enter slots 0, 1, 2.
	hyalineEnter(&h, 0)
	hyalineEnter(&h, 1)
	hyalineEnter(&h, 2)

	// Create and flush a batch.
	var batch hyalineBatch
	hyalineBatchInit(&batch)
	var freed []uint64
	fn := testFreeFn(&freed)

	// Add 5 nodes.
	for i := range 5 {
		hyalineRetire(&h, &batch, testNode(base, i), fn)
	}

	// Force flush (threshold is 65).
	hyalineRetireFlush(&h, &batch, fn)

	// Batch should NOT be freed yet — 3 slots are occupied.
	if len(freed) != 0 {
		t.Fatalf("before leave: expected 0 freed, got %d", len(freed))
	}

	// After all occupied slots leave, batch should be freed.
	for i := range 3 {
		hyalineLeave(&h, i, fn)
	}

	if len(freed) != 1 {
		t.Fatalf("after all leaves: expected 1 freed batch head, got %d", len(freed))
	}
}

func TestHyalineStaggeredLeave(t *testing.T) {
	// Slot 0 leaves early, slot 1 leaves later. Batch shouldn't be freed
	// until the last occupied slot leaves.
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, (hyalineK+10)*testSlotSize)

	hyalineEnter(&h, 0)
	hyalineEnter(&h, 1)

	var batch hyalineBatch
	hyalineBatchInit(&batch)
	var freed []uint64
	fn := testFreeFn(&freed)

	// Need hyalineThreshold nodes for a valid flush.
	// Slot 0 and 1 are occupied. We need to retire at least hyalineThreshold nodes.
	for i := 0; i < hyalineThreshold; i++ {
		hyalineRetire(&h, &batch, testNode(base, i), fn)
	}

	// Flush the batch.
	hyalineRetireFlush(&h, &batch, fn)

	if len(freed) != 0 {
		t.Fatalf("before any leave: expected 0 freed, got %d", len(freed))
	}

	// Slot 0 leaves.
	hyalineLeave(&h, 0, fn)
	if len(freed) != 0 {
		t.Fatalf("after slot 0 leave: expected 0 freed, got %d", len(freed))
	}

	// Slot 1 leaves — now batch should be freed.
	hyalineLeave(&h, 1, fn)
	if len(freed) != 1 {
		t.Fatalf("after slot 1 leave: expected 1 freed batch head, got %d (batch refs may be nonzero)", len(freed))
	}
}

func TestHyalineConcurrentEnterLeave(t *testing.T) {
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, 64*1024) // 64KB

	const goroutines = 8
	const iters = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	var errCount atomic.Int32

	for g := range goroutines {
		go func(slotIdx int) {
			defer wg.Done()
			for range iters {
				hyalineEnter(&h, slotIdx)
				// Simulate work: read some memory.
				_ = *(*byte)(unsafe.Add(base, uintptr(slotIdx)*testSlotSize))
				var freed []uint64
				hyalineLeave(&h, slotIdx, testFreeFn(&freed))
				// No batches are retired, so nothing should be freed.
				if len(freed) != 0 {
					errCount.Add(1)
				}
			}
		}(g % 8)
	}
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("%d unexpected frees during enter/leave", errCount.Load())
	}
}

func TestHyalineBatchFlushThreshold(t *testing.T) {
	// Verify that batches auto-flush at the threshold.
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, hyalineK*testSlotSize*2)

	hugeBatch := hyalineK * 2 // more than threshold

	var batch hyalineBatch
	hyalineBatchInit(&batch)
	var freed []uint64
	fn := testFreeFn(&freed)

	for i := range hugeBatch {
		hyalineRetire(&h, &batch, testNode(base, i), fn)
	}

	// Should have auto-flushed at least once.
	if len(freed) > 0 {
		t.Logf("auto-flush occurred: %d batch heads freed", len(freed))
	}

	// Batch should be empty or partially filled after auto-flush.
	if batch.counter >= hyalineThreshold {
		t.Fatalf("batch counter = %d after hugeBatch, should be < threshold=%d", batch.counter, hyalineThreshold)
	}
}

func TestHyalineZeroHeapAllocs(t *testing.T) {
	var h hyalineHeader
	hyalineHeaderInit(&h)
	base := testBase(t, (hyalineK+10)*testSlotSize)

	var batch hyalineBatch
	hyalineBatchInit(&batch)
	var freed []uint64
	fn := testFreeFn(&freed)

	// Warm up: fill and flush once to allocate the freed slice.
	hyalineEnter(&h, 0)
	for i := range hyalineThreshold {
		hyalineRetire(&h, &batch, testNode(base, i), fn)
	}
	hyalineLeave(&h, 0, fn)
	freed = freed[:0]
	batch.counter = 0
	batch.first = nil

	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			hyalineEnter(&h, 0)
			hyalineLeave(&h, 0, fn)
		}
	})

	if result.AllocsPerOp() > 0 {
		t.Errorf("enter/leave cycle: got %d allocs/op, want 0", result.AllocsPerOp())
	}
}
