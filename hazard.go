// Package memory — hazard pointer registry and reclamation.
//
// Each shard owns K=2 hazard slots where goroutines publish pointers they are
// actively reading. Before a retired slot can be reused, the scan verifies no
// hazard slot references it. This guarantees safe memory reclamation even when
// one goroutine frees a slot while another is still reading it.
//
// The design follows Maged Michael's hazard pointer algorithm:
//   - Protect publishes a pointer to the current shard's hazard slot
//   - Retire appends a freed slot to the shard's private retirement list
//   - When the global free list runs dry, scan reclaims retired slots that are
//     not protected by any hazard pointer
//
// Hazard slots use uintptr (not unsafe.Pointer) to avoid Go GC badPointer
// panics — the GC bitmap treats uintptr as a scalar and skips tracing.

package memory

import (
	"sync/atomic"
	"unsafe"
)

// HazardGuard is a token returned by Protect, used to release a hazard slot.
// The caller must hold exactly one HazardGuard at a time per protected slot.
type HazardGuard struct {
	shard int
	slot  int
}

// Protect publishes a slot pointer to the calling goroutine's hazard registry.
// While protected, the slot is guaranteed not to be reclaimed — even if another
// goroutine calls Retire on it. Returns false if both hazard slots for this
// shard are occupied; the caller must Unprotect another slot first.
//
// After Protect returns, the caller MUST validate that the slot is still
// reachable in its data structure before reading slot data. This Store-Load
// ordering is guaranteed by the atomic CAS in Protect (STLR on ARM64, XCHG on
// x86_64 — both are full Store-Load barriers).
func (sfl *ShardedFreeList) Protect(slot []byte) (HazardGuard, bool) {
	startShardIdx := getShard(sfl.numShards)
	ptr := uintptr(unsafe.Pointer(unsafe.SliceData(slot)))

	for i := 0; i < sfl.numShards; i++ {
		shardIdx := (startShardIdx + i) & (sfl.numShards - 1)
		sh := &sfl.shards[shardIdx]

		for j := 0; j < len(sh.hazards); j++ {
			if sh.hazards[j].CompareAndSwap(0, uint64(ptr)) {
				return HazardGuard{shard: shardIdx, slot: j}, true
			}
		}
	}
	return HazardGuard{}, false
}

// Unprotect clears a hazard slot previously acquired via Protect.
// The caller must ensure the guard is still valid.
func (sfl *ShardedFreeList) Unprotect(guard HazardGuard) {
	sfl.shards[guard.shard].hazards[guard.slot].Store(0)
}

// Retire defers reclamation of a slot until no hazard pointer protects it.
// Unlike Deallocate (which may immediately recycle the slot via the per-shard
// cache), Retire guarantees the slot will not be reused while any goroutine's
// hazard pointer references it.
//
// The slot is appended to the current shard's lock-free retirement stack.
// Reclamation happens during scan, which is triggered by allocation
// backpressure (when the global free list is empty).
func (sfl *ShardedFreeList) Retire(slot []byte) error {
	if len(slot) == 0 || uint64(len(slot)) != sfl.cfg.SlotSize {
		return ErrInvalidDeallocation
	}

	ptr := unsafe.Pointer(unsafe.SliceData(slot))

	var structIdx int
	var base uintptr
	fastPathOK := false
	if meta := *(*uint32)(unsafe.Add(ptr, 8)); int(unpackStructIdx(meta)) >= 0 && int(unpackStructIdx(meta)) < len(sfl.global.slabStructs) {
		si := int(unpackStructIdx(meta))
		b := uintptr(unsafe.Pointer(&sfl.global.slabStructs[si].data[0]))
		off := uintptr(ptr) - b
		if off < uintptr(sfl.cfg.SlabSize) && off%uintptr(sfl.cfg.SlotSize) == 0 {
			structIdx = si
			base = b
			fastPathOK = true
		}
	}

	if !fastPathOK {
		sfl.global.slabMu.RLock()
		structIdx, base = sfl.global.findSlabIdxLocked(ptr)
		sfl.global.slabMu.RUnlock()
		if structIdx < 0 {
			return ErrInvalidDeallocation
		}
	}

	si := sfl.global.slotIndex(ptr, base, structIdx)
	if sfl.global.slotGen[si].Swap(0) == 0 {
		return ErrDoubleDeallocation
	}

	slotSize := sfl.cfg.SlotSize
	for {
		allocated := sfl.global.allocated.Load()
		if allocated < slotSize {
			sfl.global.allocated.Store(0)
			break
		}
		if sfl.global.allocated.CompareAndSwap(allocated, allocated-slotSize) {
			break
		}
	}

	// Repack metadata so the scan can recover structIdx from offset 8.
	currentShard := getShard(sfl.numShards)
	*(*uint32)(unsafe.Add(ptr, 8)) = packSlotMeta(int32(structIdx), uint8(currentShard))

	sh := &sfl.shards[currentShard]
	sh.retired.push(ptr)
	return nil
}

// scan reclaims retired slots that are no longer protected by any hazard
// pointer. It drains all shards' retirement stacks, checks each slot against
// the global hazard snapshot, and pushes safe slots to the global FreeList.
// Unsafe slots are returned to their shard's retirement stack for the next scan.
//
// Returns the number of slots reclaimed.
func (sfl *ShardedFreeList) scan() int {
	hazards := collectHazards(sfl)
	hazardSet := toHazardSet(hazards)

	reclaimed := 0
	for i := range sfl.shards {
		nodes := sfl.shards[i].retired.drain()
		if len(nodes) == 0 {
			continue
		}

		var keep []unsafe.Pointer
		for _, ptr := range nodes {
			if _, protected := hazardSet[uintptr(ptr)]; protected {
				keep = append(keep, ptr)
			} else {
				meta := *(*uint32)(unsafe.Add(ptr, 8))
				structIdx := int(unpackStructIdx(meta))
				sfl.global.pushFree(ptr, int32(structIdx))
				reclaimed++
			}
		}

		for _, ptr := range keep {
			sfl.shards[i].retired.push(ptr)
		}
	}
	return reclaimed
}

// collectHazards returns all non-zero hazard pointers across all shards.
// The returned slice is a snapshot; concurrently published hazard pointers
// may not be visible to the caller.
func collectHazards(sfl *ShardedFreeList) []uintptr {
	hazards := make([]uintptr, 0, sfl.numShards*2)
	for i := range sfl.shards {
		for j := range sfl.shards[i].hazards {
			if ptr := sfl.shards[i].hazards[j].Load(); ptr != 0 {
				hazards = append(hazards, uintptr(ptr))
			}
		}
	}
	return hazards
}

// toHazardSet builds a lookup set from a hazard pointer slice.
// Uses a simple Go map — the slice is small (H = numShards × 2, ≤ 128 for
// typical deployments). The linear scan in collectHazards is O(H), and map
// construction is O(H). Point lookups for each retired node are O(1).
func toHazardSet(hazards []uintptr) map[uintptr]struct{} {
	set := make(map[uintptr]struct{}, len(hazards))
	for _, h := range hazards {
		set[h] = struct{}{}
	}
	return set
}

// retiredStack is a lock-free Treiber stack for retired slot pointers.
// Unlike shardCache, it does not need ABA protection — nodes are drained in
// batch by scan and individual pops never happen. The int32 len field enables
// fast threshold checks without draining.
type retiredStack struct {
	head atomic.Uint64 // pointer to head node (no ABA tag)
	len  atomic.Int32
}

func (r *retiredStack) push(ptr unsafe.Pointer) {
	for {
		old := r.head.Load()
		atomic.StoreUint64((*uint64)(ptr), old)
		if r.head.CompareAndSwap(old, uint64(uintptr(ptr))) {
			r.len.Add(1)
			return
		}
	}
}

// drain atomically removes all nodes from the stack and returns them.
// Returns nil if the stack is empty. Pre-allocates from len counter to
// avoid slice growth churn during the walk.
func (r *retiredStack) drain() []unsafe.Pointer {
	for {
		old := r.head.Load()
		if old == 0 {
			return nil
		}
		if r.head.CompareAndSwap(old, 0) {
			n := r.len.Swap(0)
			nodes := make([]unsafe.Pointer, 0, n)
			ptr := unsafe.Pointer(uintptr(old))
			for ptr != nil {
				next := unsafe.Pointer(uintptr(atomic.LoadUint64((*uint64)(ptr))))
				atomic.StoreUint64((*uint64)(ptr), 0)
				nodes = append(nodes, ptr)
				ptr = next
			}
			return nodes
		}
	}
}

// retiredCount returns the total number of retired slots across all shards.
func (sfl *ShardedFreeList) retiredCount() int {
	n := 0
	for i := range sfl.shards {
		n += int(sfl.shards[i].retired.len.Load())
	}
	return n
}
