// Package memory — Hyaline safe memory reclamation (PLDI 2021).
//
// Hyaline replaces hazard pointers for the ShardedFreeList. Reference counting
// happens only during reclamation, not during object access. The hot path
// (enter) is a single atomic store with no fence or CAS.
//
// This implements the single-width CAS variant (lfsmr_cas1.h). In this variant:
//
//   - enter stores 0x1 to the slot (occupied flag, no pointer tracking)
//   - retire queues nodes into occupied slots via CAS, increments batch refs
//   - leave drains all queued nodes, decrements batch refs, frees when zero
//
// Reference counting model (CAS1):
//
//	refs starts at 0 in the batch-head node (the first node added to the batch,
//	a.k.a. batch.last). When a batch is retired, refs += (number of slots that
//	were occupied and received a node from this batch). Each leave that drains
//	a node from this batch does fetch_sub(1) on refs. When refs reaches 0,
//	all slots have acknowledged and the batch is safe to free.
//
//	If no slots are occupied at retire time (adjs == 0), the batch is freed
//	immediately — no goroutine could be accessing the nodes.
//
// The key guarantee: a goroutine that enters slot X before retire and leaves
// after retire will drain the nodes queued to slot X during its leave. Nodes
// are never freed until all counted slots have acknowledged via leave.

package memory

import (
	"sync/atomic"
	"unsafe"
)

// hyalineOrder is log2(number of slots). k = 2^order = 64 slots.
const hyalineOrder = 6

// hyalineK is the number of Hyaline vector slots.
const hyalineK = 1 << hyalineOrder

// hyalineThreshold is the batch flush threshold. k+1 ensures at least one
// node per slot on average when flushing.
const hyalineThreshold = hyalineK + 1

// hyalineSlot is a single Hyaline vector slot, cache-line padded.
//
// State encoding:
//
//	0x0         — slot is free (no reader, no queued nodes)
//	0x1         — slot occupied (reader active, no queued nodes)
//	node | 0x1  — slot occupied + queued node chain at node
//	node         — slot not occupied, nodes queued (being drained by leave)
type hyalineSlot struct {
	_    [64]byte
	head atomic.Uint64
}

// hyalineHeader manages k Hyaline slots shared across all shards.
type hyalineHeader struct {
	slots [hyalineK]hyalineSlot
}

// hyalineHeaderInit zeros all slots in the header.
func hyalineHeaderInit(h *hyalineHeader) {
	for i := range h.slots {
		h.slots[i].head.Store(0)
	}
}

// hyalineEnter marks a slot as occupied. The hot path is a single seq_cst store.
func hyalineEnter(h *hyalineHeader, slotIdx int) {
	h.slots[slotIdx].head.Store(0x1)
}

// ptrAt is a helper that loads a uint64 from off-heap memory at ptr+offset
// and converts it to unsafe.Pointer. This is the materialization point for
// pointers stored in off-heap node metadata.
func ptrAt(ptr unsafe.Pointer, offset uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(*(*uint64)(unsafe.Add(ptr, offset))))
}

// storePtr writes a pointer as uint64 at ptr+offset.
func storePtr(ptr unsafe.Pointer, offset uintptr, val unsafe.Pointer) {
	*(*uint64)(unsafe.Add(ptr, offset)) = uint64(uintptr(val))
}

// hyalineLeave clears the occupied flag and drains any queued retired nodes.
func hyalineLeave(h *hyalineHeader, slotIdx int, freeFn func(batchHead unsafe.Pointer)) {
	slot := &h.slots[slotIdx]

	curr := slot.head.Swap(0) &^ 0x1
	if curr == 0 {
		return
	}

	var freeList unsafe.Pointer
	for curr != 0 {
		// Materialize node pointer from the slot's uint64 value.
		nodePtr := unsafe.Pointer(uintptr(curr))

		next := *(*uint64)(nodePtr)                // offset 0: next in chain
		batchHead := ptrAt(nodePtr, 8)             // offset 8: batch_head → batch head
		refsPtr := (*int64)(unsafe.Add(batchHead, 24)) // offset 24: refs

		if atomic.AddInt64(refsPtr, -1) == 0 {
			storePtr(batchHead, 0, freeList)
			freeList = batchHead
		}

		curr = next
	}

	for freeList != nil {
		batchHead := freeList
		freeList = ptrAt(batchHead, 0) // offset 0: next in free list
		freeFn(batchHead)
	}
}

// hyalineBatch is a per-shard accumulation buffer for retired nodes.
type hyalineBatch struct {
	first   unsafe.Pointer // most-recently-added node
	last    unsafe.Pointer // first-added node (batch head)
	counter uint64
}

// hyalineBatchInit resets a batch to empty.
func hyalineBatchInit(b *hyalineBatch) {
	b.first = nil
	b.counter = 0
}

// hyalineRetire appends a node to the per-shard batch.
func hyalineRetire(h *hyalineHeader, batch *hyalineBatch, node unsafe.Pointer, freeFn func(batchHead unsafe.Pointer)) {
	if batch.first == nil {
		batch.last = node
		// Initialize refs to 0 (offset 24). Previously this was implicitly zeroed 
		// because refs shared the batch_next field, which got set to batch.first (nil).
		*(*int64)(unsafe.Add(node, 24)) = 0
	}
	
	// Unconditionally set batch_head at offset 8 to batch.last
	storePtr(node, 8, batch.last) // offset 8: batch_head → batch.last
	storePtr(node, 16, batch.first) // offset 16: batch_next → previous first
	batch.first = node
	batch.counter++

	// Default flush threshold for amortized performance.
	if batch.counter >= hyalineThreshold {
		hyalineRetireFlush(h, batch, freeFn)
	}
}

// hyalineRetireFlush distributes the accumulated batch across all k slots.
func hyalineRetireFlush(h *hyalineHeader, batch *hyalineBatch, freeFn func(batchHead unsafe.Pointer)) {
	if batch.counter == 0 {
		return
	}

	// Decouple batch.first from batch.last's traversal pointer.
	// Store batch.first in offset 32 so freeFn can traverse the batch.
	storePtr(batch.last, 32, batch.first)

	var adjs int64
	curr := batch.first

	for i := 0; i < hyalineK; i++ {
		slot := &h.slots[i]

		for {
			old := slot.head.Load()
			if old&0x1 == 0 {
				break
			}

			newVal := uint64(uintptr(curr)) | 0x1
			// Write the old chain head as the node's next pointer.
			*(*uint64)(curr) = old &^ 0x1 // offset 0: next

			if slot.head.CompareAndSwap(old, newVal) {
				adjs++
				curr = ptrAt(curr, 16) // offset 16: batch_next
				if curr == nil {
					goto adjust
				}
				break
			}
		}
	}

adjust:
	refsPtr := (*int64)(unsafe.Add(batch.last, 24))
	newRefs := atomic.AddInt64(refsPtr, adjs)

	if newRefs == 0 {
		freeFn(batch.last)
	}

	hyalineBatchInit(batch)
}
