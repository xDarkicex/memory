package memory

import (
	"unsafe"
)

const maxProbeBuckets = uint64(32)

// hashKey mixes the uint64 key to produce a high-entropy 64-bit hash.
func hashKey(k uint64) uint64 {
	return k * 0x9e3779b97f4a7c15
}

func bucketIndex(s *mapState, hash uint64) uint64 {
	return hash & (s.size - 1)
}

func loadBucketMeta(b *Bucket) uint64 {
	return *(*uint64)(unsafe.Pointer(&b.Metadata))
}

func loadBucketKey(b *Bucket, idx uint32) uint64 {
	return *(*uint64)(unsafe.Pointer(&b.Keys[idx]))
}

func loadBucketVal(b *Bucket, idx uint32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&b.Vals[idx]))
}

func storeBucketKey(b *Bucket, idx uint32, key uint64) {
	*(*uint64)(unsafe.Pointer(&b.Keys[idx])) = key
}

func storeBucketVal(b *Bucket, idx uint32, val unsafe.Pointer) {
	*(*uintptr)(unsafe.Pointer(&b.Vals[idx])) = uintptr(val)
}

// Put inserts a key and value into the map. val must be an off-heap pointer
// (Arena, FreeList, Pool, or ShardedFreeList allocation). The map does not keep
// Go heap pointers alive — the GC never scans the mmap'd bucket array.
func (h *HashMap) Put(key uint64, val unsafe.Pointer) {
	for {
		s := h.state.Load()
		if s.next != nil {
			h.helpMigrateAll(s)
			continue
		}

		if err := h.putInner(s, key, val); err == nil {
			return
		} else if err == ErrNeedsResize {
			curr := h.state.Load()
			if curr != s || curr.next != nil {
				continue
			}
			_ = h.triggerResize()
		}
	}
}

// putInner inserts into a specific generation state using linear probing.
func (h *HashMap) putInner(s *mapState, key uint64, val unsafe.Pointer) error {
	hash := hashKey(key)
	h2 := uint8(hash >> 56)
	startIdx := bucketIndex(s, hash)
	probeLimit := s.size
	if probeLimit > maxProbeBuckets {
		probeLimit = maxProbeBuckets
	}

	for i := uint64(0); i < probeLimit; i++ {
		bucketIdx := (startIdx + i) & (s.size - 1)
		b := (*Bucket)(unsafe.Pointer(uintptr(s.base) + uintptr(bucketIdx*128)))

		for {
			meta := b.Metadata.Load()
			_, _, _, _, migrating := extractMasks(meta)

			if migrating || meta&bucketMigratedBit != 0 {
				return ErrNeedsResize // Outer loop will helpMigrate
			}

			// First, check if key already exists in this bucket to update it
			match := matchMask(meta, h2)
			for match != 0 {
				idx := firstMatch(match)
				if b.Keys[idx].Load() == key {
					// Overwrite existing value
					storeBucketVal(b, idx, val)
					return nil
				}
				match &= match - 1
			}

			empty := emptyMask(meta)
			if empty == 0 {
				break // Bucket is full, go to next bucket (linear probe)
			}

			if meta&(uint64(0x7F)<<21) != 0 {
				return h.putInnerTombstone(s, key, val, h2, startIdx, probeLimit)
			}

			idx := firstMatch(empty)

			// Claim the slot: set O bit, clear P bit (tombstone) if it was set
			newMeta := meta | (1 << idx)  // set O
			newMeta &^= (1 << (21 + idx)) // clear P

			// Set fingerprint
			newMeta &^= (0x1F << (29 + idx*5))
			newMeta |= (uint64(h2&0x1F) << (29 + idx*5))

			if b.Metadata.CompareAndSwap(meta, newMeta) {
				storeBucketKey(b, idx, key)
				storeBucketVal(b, idx, val)
				return nil
			}
			// CAS failed, retry this bucket
		}
	}
	return ErrNeedsResize
}

func (h *HashMap) putInnerTombstone(s *mapState, key uint64, val unsafe.Pointer, h2 uint8, startIdx, probeLimit uint64) error {
	var tombBucket *Bucket
	var tombIdx uint32

	for i := uint64(0); i < probeLimit; i++ {
		bucketIdx := (startIdx + i) & (s.size - 1)
		b := (*Bucket)(unsafe.Pointer(uintptr(s.base) + uintptr(bucketIdx*128)))

		for {
			meta := b.Metadata.Load()
			_, _, _, _, migrating := extractMasks(meta)

			if migrating || meta&bucketMigratedBit != 0 {
				return ErrNeedsResize
			}

			match := matchMask(meta, h2)
			for match != 0 {
				idx := firstMatch(match)
				if b.Keys[idx].Load() == key {
					storeBucketVal(b, idx, val)
					return nil
				}
				match &= match - 1
			}

			empty := emptyMask(meta)
			if empty == 0 {
				break
			}

			P := uint8((meta >> 21) & 0x7F)
			tombs := P & empty
			if tombs != 0 && tombBucket == nil {
				tombBucket = b
				tombIdx = firstMatch(tombs)
			}

			trueEmpty := empty &^ P
			if trueEmpty == 0 {
				break
			}

			if tombBucket != nil {
				reuseMeta := tombBucket.Metadata.Load()
				oBit := uint64(1) << tombIdx
				pBit := uint64(1) << (21 + tombIdx)
				if reuseMeta&oBit == 0 && reuseMeta&pBit != 0 && reuseMeta&bucketMigratingBit == 0 && reuseMeta&bucketMigratedBit == 0 {
					newMeta := reuseMeta | oBit
					newMeta &^= pBit
					newMeta &^= (0x1F << (29 + tombIdx*5))
					newMeta |= uint64(h2&0x1F) << (29 + tombIdx*5)
					if tombBucket.Metadata.CompareAndSwap(reuseMeta, newMeta) {
						storeBucketKey(tombBucket, tombIdx, key)
						storeBucketVal(tombBucket, tombIdx, val)
						return nil
					}
				}
				tombBucket = nil
				continue
			}

			idx := firstMatch(trueEmpty)
			newMeta := meta | (1 << idx)
			newMeta &^= (1 << (21 + idx))
			newMeta &^= (0x1F << (29 + idx*5))
			newMeta |= uint64(h2&0x1F) << (29 + idx*5)
			if b.Metadata.CompareAndSwap(meta, newMeta) {
				storeBucketKey(b, idx, key)
				storeBucketVal(b, idx, val)
				return nil
			}
		}
	}
	return ErrNeedsResize
}

// Get retrieves a value from the map. The returned pointer is the same off-heap
// pointer that was passed to Put.
func (h *HashMap) Get(key uint64) (unsafe.Pointer, bool) {
	for {
		s := h.state.Load()
		if s.next != nil {
			hash := hashKey(key)
			h.helpMigrate(s, bucketIndex(s, hash))
			if ptr, found := h.getInner(s.next, key); found {
				return ptr, true
			}
			return h.getInner(s, key)
		}
		return h.getInner(s, key)
	}
}

func (h *HashMap) getInner(s *mapState, key uint64) (unsafe.Pointer, bool) {
	hash := hashKey(key)
	h2 := uint8(hash >> 56)
	startIdx := bucketIndex(s, hash)
	probeLimit := s.size
	if probeLimit > maxProbeBuckets {
		probeLimit = maxProbeBuckets
	}

	for i := uint64(0); i < probeLimit; i++ {
		bucketIdx := (startIdx + i) & (s.size - 1)
		basePtr := s.base
		b := (*Bucket)(unsafe.Pointer(uintptr(basePtr) + uintptr(bucketIdx*128)))

		meta := loadBucketMeta(b)
		match := matchMask(meta, h2)
		for match != 0 {
			idx := firstMatch(match)
			if loadBucketKey(b, idx) == key {
				val := loadBucketVal(b, idx)
				return unsafe.Pointer(val), true
			}
			match &= match - 1
		}

		// Stop probing if we hit a bucket that is not completely full.
		// Wait, a slot might be empty but we must continue if there are tombstones.
		// Actually, standard linear probing stops if it hits a completely unallocated slot.
		// If emptyMask > 0, does it mean we stop?
		// Only if it's truly empty and NOT a tombstone.
		O := uint8(meta & 0x7F)
		P := uint8((meta >> 21) & 0x7F)

		// If there is any slot that is NEITHER occupied NOR a tombstone, we can stop probing.
		// O | P gives slots that are either occupied or tombstones.
		// If (O | P) != 0x7F, then there is at least one truly empty slot.
		if (O | P) != 0x7F {
			break
		}
	}
	return nil, false
}

// Delete removes a value from the map using tombstones. The caller is responsible
// for freeing the off-heap value separately — Delete only removes the map entry.
func (h *HashMap) Delete(key uint64) bool {
	for {
		s := h.state.Load()
		if s.next != nil {
			h.helpMigrateAll(s)
			continue
		}

		deleted := h.deleteInner(s, key)
		if !deleted && h.state.Load() != s {
			continue
		}
		return deleted
	}
}

func (h *HashMap) deleteInner(s *mapState, key uint64) bool {
	hash := hashKey(key)
	h2 := uint8(hash >> 56)
	startIdx := bucketIndex(s, hash)
	probeLimit := s.size
	if probeLimit > maxProbeBuckets {
		probeLimit = maxProbeBuckets
	}

	for i := uint64(0); i < probeLimit; i++ {
		bucketIdx := (startIdx + i) & (s.size - 1)
		b := (*Bucket)(unsafe.Pointer(uintptr(s.base) + uintptr(bucketIdx*128)))

		for {
			meta := b.Metadata.Load()
			_, _, _, _, migrating := extractMasks(meta)

			if migrating || meta&bucketMigratedBit != 0 {
				return false
			}

			match := matchMask(meta, h2)
			foundMatchInBucket := false
			for match != 0 {
				idx := firstMatch(match)
				if b.Keys[idx].Load() == key {
					foundMatchInBucket = true
					// Clear O bit, set P bit (Tombstone)
					newMeta := meta &^ (1 << idx) // clear O
					newMeta |= (1 << (21 + idx))  // set P

					if b.Metadata.CompareAndSwap(meta, newMeta) {
						return true
					}
					break // CAS failed, retry this bucket
				}
				match &= match - 1
			}

			if foundMatchInBucket {
				// We broke out of the match loop due to CAS failure, retry the same bucket
				continue
			}

			// If we get here, the key is not in this bucket.
			// Should we continue probing?
			O := uint8(meta & 0x7F)
			P := uint8((meta >> 21) & 0x7F)
			if (O | P) != 0x7F {
				return false // Hit a truly empty slot, key doesn't exist
			}
			break // Break inner loop to go to next bucket
		}
	}
	return false
}
