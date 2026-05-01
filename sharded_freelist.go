// Package memory — sharded hazard-pointer allocator.
//
// ShardedFreeList wraps a global FreeList with per-shard LIFO caches.
// The hot path (same-shard alloc/free) has zero atomics. Deallocate always
// routes to the current goroutine's shard, keeping slots on the local CPU.
// The global FreeList provides batch refills and slab management.

package memory

import (
	"sync/atomic"
	"unsafe"
)

// ShardedFreeList is a sharded, lock-free, fixed-size off-heap allocator.
// N shards each own LIFO caches backed by a shared FreeList for batch refills.
type ShardedFreeList struct {
	cfg       FreeListConfig
	global    *FreeList
	shards    []shard
	numShards int
	gen       atomic.Uint64
}

type shard struct {
	_        [64]byte          // Padding to prevent false sharing
	recycled shardCache        // Slots from Deallocate (need activateSlot on pop)
	fresh    freshCache        // Slots from BatchAllocate (already accounted)
	hazards  [2]atomic.Uint64  // K=2 hazard pointer slots (uintptr as uint64)
	retired  retiredStack      // Lock-free retirement list for HP-protected frees
}

// NewShardedFreeList creates a sharded allocator with numShards shards.
// If numShards <= 0, defaults to GOMAXPROCS.
func NewShardedFreeList(cfg FreeListConfig, numShards int) (*ShardedFreeList, error) {
	if numShards <= 0 {
		numShards = 8
	}
	if numShards&(numShards-1) != 0 {
		n := 1
		for n < numShards {
			n <<= 1
		}
		numShards = n
	}

	global, err := NewFreeList(cfg)
	if err != nil {
		return nil, err
	}

	shards := make([]shard, numShards)
	sfl := &ShardedFreeList{
		cfg:       cfg,
		global:    global,
		shards:    shards,
		numShards: numShards,
	}
	return sfl, nil
}

// activateSlot sets the double-free guard and allocated counter for a slot
// popped from recycled. The slot's metadata at offset 8 contains structIdx
// in the lower 24 bits, repacked by Deallocate so it survives user writes.
func (sfl *ShardedFreeList) activateSlot(ptr unsafe.Pointer) {
	meta := *(*uint32)(unsafe.Add(ptr, 8))
	structIdx := int(unpackStructIdx(meta))
	base := uintptr(unsafe.Pointer(&sfl.global.slabStructs[structIdx].data[0]))
	si := sfl.global.slotIndex(ptr, base, structIdx)
	
	// We use a simple bitwise or local atomic instead of the global allocSeq
	// to avoid massive global cache line bouncing on every allocation.
	sfl.global.slotGen[si].Store(1)
}

// setHomeShard writes the shard index into the slot's packed metadata without
// disturbing the structIdx field (lower 24 bits).
func setHomeShard(ptr unsafe.Pointer, shardIdx uint8) {
	meta := *(*uint32)(unsafe.Add(ptr, 8))
	*(*uint32)(unsafe.Add(ptr, 8)) = packSlotMeta(unpackStructIdx(meta), shardIdx)
}

// Allocate returns a fixed-size slot.
// It uses a scalable cross-shard scanning mechanism:
// 1. Picks a fastrand-based starting shard.
// 2. Scans all local shards in sequence for `fresh` or `recycled` slots.
// 3. If all local caches are empty, performs a batch refill from the global FreeList.
// 4. If the global FreeList is empty, triggers a hazard pointer retirement scan.
func (sfl *ShardedFreeList) Allocate() ([]byte, error) {
	gen := sfl.gen.Load()
	startShardIdx := getShard(sfl.numShards)
	slotSize := sfl.cfg.SlotSize

	for i := 0; i < sfl.numShards; i++ {
		shardIdx := (startShardIdx + i) & (sfl.numShards - 1)
		sh := &sfl.shards[shardIdx]

		// 1. Fresh cache: slots from BatchAllocate, already accounted.
		if ptr := sh.fresh.pop(); ptr != nil {
			if sfl.gen.Load() != gen {
				goto retry
			}
			setHomeShard(ptr, uint8(shardIdx))
			return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
		}

		// 2. Recycled cache: slots from Deallocate, need activateSlot.
		if ptr := sh.recycled.pop(); ptr != nil {
			if sfl.gen.Load() != gen {
				goto retry
			}
			sfl.activateSlot(ptr)
			setHomeShard(ptr, uint8(shardIdx))
			return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
		}
	}

	// 3. Batch refill from global FreeList.
	{
		var slots [batchSize][]byte
		count, err := sfl.global.BatchAllocate(slots[:])
		if count == 0 {
			// 4. Global FreeList is empty — try to reclaim retired slots.
			//    This catches both genuine emptiness and pool-exhaustion
			//    errors from growSlab when retired slots exist.
			//
			//    Retry once if scan finds nothing: another goroutine may
			//    be mid-scan and about to publish reclaimed slots to the
			//    global FreeList. The second BatchAllocate picks them up.
			reclaimed := sfl.scan()
			if reclaimed > 0 {
				goto retry
			}
			count2, err2 := sfl.global.BatchAllocate(slots[:])
			if count2 > 0 {
				count = count2
				err = err2
				goto fill
			}
			if err2 != nil {
				return nil, err2
			}
			if err != nil {
				return nil, err
			}
			return nil, ErrFreelistExhausted
		}
	fill:
		if err != nil {
			return nil, err
		}

		homeSh := &sfl.shards[startShardIdx]
		for i := 1; i < count; i++ {
			ptr := unsafe.Pointer(unsafe.SliceData(slots[i]))
			setHomeShard(ptr, uint8(startShardIdx))
			homeSh.fresh.push(ptr)
		}

		ptr := unsafe.Pointer(unsafe.SliceData(slots[0]))
		setHomeShard(ptr, uint8(startShardIdx))
		return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
	}

retry:
	return sfl.Allocate()
}

// Deallocate returns a slot to the sharded caches.
// It implements an O(1) lock-free fast path by reading slot metadata at offset 8,
// bypassing the global binary search entirely.
// To prevent cache exhaustion, it attempts to push the slot onto the current random
// shard's recycled stack. If full, it scans adjacent shards. It only falls back to
// the global FreeList when all local caches are completely saturated.
func (sfl *ShardedFreeList) Deallocate(slot []byte) error {
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

	currentShard := getShard(sfl.numShards)

	for i := 0; i < sfl.numShards; i++ {
		shardIdx := (currentShard + i) & (sfl.numShards - 1)
		*(*uint32)(unsafe.Add(ptr, 8)) = packSlotMeta(int32(structIdx), uint8(shardIdx))

		if sfl.shards[shardIdx].recycled.push(ptr) {
			return nil
		}
	}

	// Fast paths failed. Slot is going back to the global FreeList.
	// Now we must decrement global allocated.
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

	*(*uint32)(unsafe.Add(ptr, 8)) = packSlotMeta(int32(structIdx), uint8(currentShard))
	sfl.global.pushFree(ptr, int32(structIdx))
	return nil
}

// Reset releases all in-flight slots and reinitializes shards.
// WARNING: Not concurrent-safe. Caller must ensure quiescence.
func (sfl *ShardedFreeList) Reset() {
	sfl.gen.Add(1)
	sfl.global.Reset()
	for i := range sfl.shards {
		sfl.shards[i].recycled.head.Store(0)
		sfl.shards[i].recycled.len.Store(0)
		sfl.shards[i].fresh.head.Store(0)
		sfl.shards[i].fresh.len.Store(0)
		sfl.shards[i].retired.head.Store(0)
		sfl.shards[i].retired.len.Store(0)
		for j := range sfl.shards[i].hazards {
			sfl.shards[i].hazards[j].Store(0)
		}
	}
}

// Free releases all resources. The allocator must not be used after Free.
func (sfl *ShardedFreeList) Free() error {
	sfl.gen.Add(1)
	return sfl.global.Free()
}

// Stats returns a point-in-time snapshot of allocator state.
func (sfl *ShardedFreeList) Stats() FreeListStats {
	return sfl.global.Stats()
}
