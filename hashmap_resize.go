package memory

import (
	"unsafe"
)

// helpMigrate implements Dr. Cliff Click's cooperative migration state machine.
// Any mutator that encounters an active resize (s.next != nil) actively jumps
// in to migrate its target bucket concurrently using wait-free CAS.
//
//go:nosplit
func (h *HashMap) helpMigrate(s *mapState, bucketIdx uint64) {
	if s.next == nil {
		return
	}
	b := (*Bucket)(unsafe.Pointer(uintptr(s.base) + uintptr(bucketIdx*128)))
	for {
		meta := b.Metadata.Load()
		if meta&bucketMigratedBit != 0 {
			return
		}

		O, _, F, _, migrating := extractMasks(meta)
		if !migrating {
			newMeta := meta | bucketMigratingBit
			if !b.Metadata.CompareAndSwap(meta, newMeta) {
				continue
			}
			meta = newMeta
		}
		// If fully forwarded, we are done with this bucket.
		if F == O {
			newMeta := (meta | bucketMigratedBit) &^ bucketMigratingBit
			if b.Metadata.CompareAndSwap(meta, newMeta) {
				rem := s.bucketsRemaining.Add(^uint64(0)) // decrement
				if rem == 0 {
					// We finished migrating the entire map! Promote next to current.
					if h.state.CompareAndSwap(s, s.next) {
						// Hook into Hyaline SMR: enqueue the old mmap'd region for deferred
						// munmapRaw. Flush eagerly — the batch holds a single node so the
						// threshold (hyalineK+1) would never fire organically.
						var batch hyalineBatch
						hyalineBatchInit(&batch)

						nodePtr := unsafe.Pointer(uintptr(s.base) - 128)
						hyalineRetire(&h.smrHeader, &batch, nodePtr, hashMapSMRFreeFn)
						hyalineRetireFlush(&h.smrHeader, &batch, hashMapSMRFreeFn)
					}
				}
				return
			}
			Spin()
			continue
		}
		// Find a slot to forward (O=1, F=0)
		mask := O &^ F
		if mask == 0 {
			continue // Should not be reached because F != O
		}

		idx := firstMatch(mask)
		k := b.Keys[idx].Load()
		v := b.Vals[idx].Load()
		// Wait-free insert into the new generation
		// s.next has double capacity, load factor < 0.5, so 32-hop bound is mathematically guaranteed to succeed.
		if err := h.putInner(s.next, k, unsafe.Pointer(v)); err != nil {
			Spin()
			continue
		}
		// Mark this slot as forwarded in the old generation
		for {
			curr := b.Metadata.Load()
			if curr&bucketMigratedBit != 0 {
				return
			}
			newMeta := curr | (1 << (14 + idx)) // Set F bit
			if b.Metadata.CompareAndSwap(curr, newMeta) {
				break
			}
			Spin()
		}
	}
}

// triggerResize is invoked by the PID controller when the load factor threshold is breached.
func (h *HashMap) triggerResize() error {
	s := h.state.Load()
	if s.next != nil {
		return nil // Already migrating
	}
	capacity := s.size * 7 * 2
	if capacity < 8 {
		capacity = 8
	}
	capacity--
	capacity |= capacity >> 1
	capacity |= capacity >> 2
	capacity |= capacity >> 4
	capacity |= capacity >> 8
	capacity |= capacity >> 16
	capacity |= capacity >> 32
	capacity++
	bucketCount := (capacity + 6) / 7
	bucketCount--
	bucketCount |= bucketCount >> 1
	bucketCount |= bucketCount >> 2
	bucketCount |= bucketCount >> 4
	bucketCount |= bucketCount >> 8
	bucketCount |= bucketCount >> 16
	bucketCount |= bucketCount >> 32
	bucketCount++
	allocSize := bucketCount*128 + 128
	addr, err := mmapRawAnonymous(allocSize)
	if err != nil {
		return err
	}
	*(*uint64)(unsafe.Add(unsafe.Pointer(addr), 64)) = allocSize
	nextState := &mapState{
		base:     unsafe.Pointer(addr + 128),
		size:     bucketCount,
		mmapSize: allocSize,
	}

	// Copy current state and set next pointer
	newState := &mapState{
		base:     s.base,
		size:     s.size,
		mmapSize: s.mmapSize,
		next:     nextState,
	}
	newState.bucketsRemaining.Store(s.size)
	// Activate migration phase
	if !h.state.CompareAndSwap(s, newState) {
		_ = munmapRaw(addr, allocSize)
	}
	return nil
}

// helpMigrateAll scans all buckets in the map and helps migrate them.
func (h *HashMap) helpMigrateAll(s *mapState) {
	if s.next == nil {
		return
	}
	for i := uint64(0); i < s.size; i++ {
		if s.bucketsRemaining.Load() == 0 {
			return
		}
		h.helpMigrate(s, i)
	}
}
