// Package memory — sharded Hyaline allocator.
//
// ShardedFreeList wraps a global FreeList with per-shard LIFO caches.
// The hot path (same-shard alloc/free) has zero atomics. Deallocate always
// routes to the current goroutine's shard, keeping slots on the local CPU.
// The global FreeList provides batch refills and slab management.
//
// Safe memory reclamation uses Hyaline (PLDI 2021) instead of hazard pointers.
// The hot path (enter) is a single atomic store with no fence or CAS.
// Reference counting happens only during reclamation, not during object access.

package memory

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ShardedFreeList is a sharded, lock-free, fixed-size off-heap allocator.
// N shards each own LIFO caches backed by a shared FreeList for batch refills.
// Safe memory reclamation is provided by Hyaline (hyaline.go).
type ShardedFreeList struct {
	cfg       FreeListConfig
	global    *FreeList
	shards    []shard
	numShards int
	gen       atomic.Uint64
	hyHeader  hyalineHeader
	cancel    context.CancelFunc
}

type shard struct {
	_        [64]byte     // Padding to prevent false sharing
	recycled shardCache   // Slots from Deallocate (need activateSlot on pop)
	fresh    freshCache   // Slots from BatchAllocate (already accounted)
	batch    hyalineBatch // Hyaline retirement batch (per-shard, mutex-protected)
	batchMu  sync.Mutex   // Protects batch; uncontended under procpin (P-bound sharding)
}

// NewShardedFreeList creates a sharded allocator with numShards shards.
// If numShards <= 0, defaults to 64 (over-provisioned to reduce hash collisions
// across GOMAXPROCS cores without requiring procpin).
func NewShardedFreeList(cfg FreeListConfig, numShards int) (*ShardedFreeList, error) {
	if numShards <= 0 {
		numShards = 64
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
	ctx, cancel := context.WithCancel(context.Background())

	sfl := &ShardedFreeList{
		cfg:       cfg,
		global:    global,
		shards:    shards,
		numShards: numShards,
		cancel:    cancel,
	}
	hyalineHeaderInit(&sfl.hyHeader)
	
	go sfl.runPIDController(ctx)
	
	return sfl, nil
}

// activateSlot sets the double-free guard for a slot popped from recycled.
// The slot's metadata at offset 40 contains structIdx in the lower 24 bits.
func (sfl *ShardedFreeList) activateSlot(ptr unsafe.Pointer) {
	meta := *(*uint32)(unsafe.Add(ptr, 40))
	structIdx := int(unpackStructIdx(meta))
	base := uintptr(unsafe.Pointer(&sfl.global.slabStructs[structIdx].data[0]))
	si := sfl.global.slotIndex(ptr, base, structIdx)
	sfl.global.slotGen[si].Store(1)
}

// setHomeShard writes the shard index into offset 40 without disturbing structIdx.
func setHomeShard(ptr unsafe.Pointer, shardIdx uint8) {
	meta := *(*uint32)(unsafe.Add(ptr, 40))
	*(*uint32)(unsafe.Add(ptr, 40)) = packSlotMeta(unpackStructIdx(meta), shardIdx)
}

// hyalineFreeFn pushes all nodes in a freed Hyaline batch back to the global
// FreeList. Each node's structIdx is read from offset 40 (preserved during
// Hyaline operations at offsets 0, 8, 16, 24, 32).
func (sfl *ShardedFreeList) hyalineFreeFn(batchHead unsafe.Pointer) {
	// Start from batch.first (stored at offset 32 of batch head after flush).
	first := ptrAt(batchHead, 32) // offset 32: first_node → batch.first
	for curr := first; curr != nil; {
		next := ptrAt(curr, 16) // offset 16: batch_next
		meta := *(*uint32)(unsafe.Add(curr, 40))
		structIdx := int(unpackStructIdx(meta))
		sfl.global.pushFree(curr, int32(structIdx))
		curr = next
	}
}

// HyalineEnter marks a Hyaline vector slot as occupied. The hot path is a
// single atomic store — no CAS, no fence. Call before reading a slot that
// may be concurrently retired. The slotIdx should be the shard index.
func (sfl *ShardedFreeList) HyalineEnter(slotIdx int) {
	hyalineEnter(&sfl.hyHeader, slotIdx&(hyalineK-1))
}

// HyalineLeave clears the occupied flag and drains any queued retired nodes.
// Batches whose reference counts reach zero are pushed back to the global
// FreeList. Call after retiring slots accessed under HyalineEnter.
func (sfl *ShardedFreeList) HyalineLeave(slotIdx int) {
	hyalineLeave(&sfl.hyHeader, slotIdx&(hyalineK-1), sfl.hyalineFreeFn)
}

// Allocate returns a fixed-size slot from the sharded allocator.
func (sfl *ShardedFreeList) Allocate() ([]byte, error) {
	gen := sfl.gen.Load()
	startShardIdx := getShard(sfl.numShards)
	slotSize := sfl.cfg.SlotSize

	for i := 0; i < sfl.numShards; i++ {
		shardIdx := (startShardIdx + i) & (sfl.numShards - 1)
		sh := &sfl.shards[shardIdx]

		if ptr := sh.fresh.pop(); ptr != nil {
			if sfl.gen.Load() != gen {
				goto retry
			}
			setHomeShard(ptr, uint8(shardIdx))
			return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
		}

		if ptr := sh.recycled.pop(); ptr != nil {
			if sfl.gen.Load() != gen {
				goto retry
			}
			sfl.activateSlot(ptr)
			setHomeShard(ptr, uint8(shardIdx))
			return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
		}
	}

	// Batch refill from global FreeList.
	{
		var slots [batchSize][]byte
		count, err := sfl.global.BatchAllocate(slots[:])
		if count == 0 {
			// Global FreeList is empty. Hyaline reclamation is continuous
			// (distributed across Leave calls), but other goroutines may
			// have just freed batches. Retry once.
			count2, err2 := sfl.global.BatchAllocate(slots[:])
			if count2 > 0 {
				count = count2
				err = err2
				goto fill
			}
			if err2 != nil {
				// Pool exhaustion: memory is likely stranded in per-shard Hyaline batches.
				// Force flush all partial batches to release stranded nodes.
				sfl.forceReclamation()
				count2, err2 = sfl.global.BatchAllocate(slots[:])
				if count2 > 0 {
					count = count2
					err = err2
					goto fill
				}
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
func (sfl *ShardedFreeList) Deallocate(slot []byte) error {
	if len(slot) == 0 || uint64(len(slot)) != sfl.cfg.SlotSize {
		return ErrInvalidDeallocation
	}

	ptr := unsafe.Pointer(unsafe.SliceData(slot))

	var structIdx int
	var base uintptr
	fastPathOK := false
	if meta := *(*uint32)(unsafe.Add(ptr, 40)); int(unpackStructIdx(meta)) >= 0 && int(unpackStructIdx(meta)) < len(sfl.global.slabStructs) {
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
		*(*uint32)(unsafe.Add(ptr, 40)) = packSlotMeta(int32(structIdx), uint8(shardIdx))

		if sfl.shards[shardIdx].recycled.push(ptr) {
			return nil
		}
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

	*(*uint32)(unsafe.Add(ptr, 40)) = packSlotMeta(int32(structIdx), uint8(currentShard))
	sfl.global.pushFree(ptr, int32(structIdx))
	return nil
}

// Retire defers reclamation of a slot via Hyaline reference counting.
// The slot is added to the calling shard's retirement batch. When the batch
// reaches the Hyaline threshold, it flushes to the global header. Reclamation
// happens when all goroutines that entered the corresponding slots have left.
func (sfl *ShardedFreeList) Retire(slot []byte) error {
	if len(slot) == 0 || uint64(len(slot)) != sfl.cfg.SlotSize {
		return ErrInvalidDeallocation
	}

	ptr := unsafe.Pointer(unsafe.SliceData(slot))

	var structIdx int
	var base uintptr
	fastPathOK := false
	if meta := *(*uint32)(unsafe.Add(ptr, 40)); int(unpackStructIdx(meta)) >= 0 && int(unpackStructIdx(meta)) < len(sfl.global.slabStructs) {
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

	// Preserve structIdx at offset 40 for the freeFn callback.
	currentShard := getShard(sfl.numShards)
	*(*uint32)(unsafe.Add(ptr, 40)) = packSlotMeta(int32(structIdx), uint8(currentShard))

	sh := &sfl.shards[currentShard]
	sh.batchMu.Lock()
	hyalineRetire(&sfl.hyHeader, &sh.batch, ptr, sfl.hyalineFreeFn)
	sh.batchMu.Unlock()
	return nil
}

// Reset releases all in-flight slots and reinitializes shards.
// WARNING: Not concurrent-safe. Caller must ensure quiescence.
func (sfl *ShardedFreeList) Reset() {
	if sfl.cancel != nil {
		sfl.cancel()
	}
	sfl.gen.Add(1)
	sfl.global.Reset()
	hyalineHeaderInit(&sfl.hyHeader)
	for i := range sfl.shards {
		sfl.shards[i].recycled.head.Store(0)
		sfl.shards[i].recycled.len.Store(0)
		sfl.shards[i].fresh.head.Store(0)
		sfl.shards[i].fresh.len.Store(0)
		hyalineBatchInit(&sfl.shards[i].batch)
	}

	// Restart the adaptive PID controller for the new lifecycle
	ctx, cancel := context.WithCancel(context.Background())
	sfl.cancel = cancel
	go sfl.runPIDController(ctx)
}

// Free releases all resources. The allocator must not be used after Free.
func (sfl *ShardedFreeList) Free() error {
	if sfl.cancel != nil {
		sfl.cancel()
	}
	sfl.gen.Add(1)
	return sfl.global.Free()
}

// Stats returns a point-in-time snapshot of allocator state.
func (sfl *ShardedFreeList) Stats() FreeListStats {
	return sfl.global.Stats()
}

// forceReclamation iterates through all shards, locks their batch mutexes,
// and force-flushes any partial batches to recover stranded nodes during
// pool exhaustion.
func (sfl *ShardedFreeList) forceReclamation() {
	for i := 0; i < sfl.numShards; i++ {
		sh := &sfl.shards[i]
		sh.batchMu.Lock()
		if sh.batch.counter > 0 {
			hyalineRetireFlush(&sfl.hyHeader, &sh.batch, sfl.hyalineFreeFn)
		}
		sh.batchMu.Unlock()
	}
	
	// Micro-optimization: Yield the processor to allow active readers a chance 
	// to call hyalineLeave, drain the slot chains, and free the memory before 
	// the allocator retries BatchAllocate.
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
}

// runPIDController runs a background PI control loop to dynamically adjust
// the hyaline batch flush threshold based on pool depth.
func (sfl *ShardedFreeList) runPIDController(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// Proportional and Integral gains
	const Kp = 2.0
	const Ki = 0.5
	
	var integral float64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := sfl.Stats()
			if stats.SlotSize == 0 || stats.Reserved == 0 {
				continue
			}

			// Calculate pool depth
			totalSlots := float64(stats.Reserved / stats.SlotSize)
			allocatedSlots := float64(stats.Allocated / stats.SlotSize)
			currentDepth := totalSlots - allocatedSlots
			
			// Target 20% free capacity
			targetDepth := totalSlots * 0.20
			
			// Error is positive when pool is below target
			err := targetDepth - currentDepth
			
			// Update integral (anti-windup by capping it)
			integral += err
			if integral > 100 {
				integral = 100
			} else if integral < -100 {
				integral = -100
			}

			// Calculate new threshold: 65 - (Kp * error + Ki * integral)
			// Positive error drives threshold down to flush sooner.
			adjustment := (Kp * err) + (Ki * integral)
			
			newThreshold := float64(65) - adjustment
			
			// Clamp between 1 and 65
			var clamped uint64
			if newThreshold > 65 {
				clamped = 65
			} else if newThreshold < 1 {
				clamped = 1
			} else {
				clamped = uint64(newThreshold)
			}
			
			sfl.hyHeader.threshold.Store(clamped)
		}
	}
}
