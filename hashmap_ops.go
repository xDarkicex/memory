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

// Put inserts a key and value into the flat array open-addressed map.
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
		bAddr := s.base + uintptr(bucketIdx)*128
		b := (*Bucket)(unsafe.Pointer(bAddr))

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
					b.Vals[idx].Store(uintptr(val))
					return nil
				}
				match &= match - 1
			}

			// If not found, try to insert into an empty or tombstone slot
			empty := emptyMask(meta)
			if empty == 0 {
				break // Bucket is full, go to next bucket (linear probe)
			}

			idx := firstMatch(empty)

			// Claim the slot: set O bit, clear P bit (tombstone) if it was set
			newMeta := meta | (1 << idx)  // set O
			newMeta &^= (1 << (21 + idx)) // clear P

			// Set fingerprint
			newMeta &^= (0x1F << (29 + idx*5))
			newMeta |= (uint64(h2&0x1F) << (29 + idx*5))

			if b.Metadata.CompareAndSwap(meta, newMeta) {
				b.Keys[idx].Store(key)
				b.Vals[idx].Store(uintptr(val))
				return nil
			}
			// CAS failed, retry this bucket
		}
	}
	return ErrNeedsResize
}

// Get retrieves a value from the map using linear probing.
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
		bAddr := s.base + uintptr(bucketIdx)*128
		b := (*Bucket)(unsafe.Pointer(bAddr))

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

// Delete removes a value from the map using linear probing and tombstones.
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
		bAddr := s.base + uintptr(bucketIdx)*128
		b := (*Bucket)(unsafe.Pointer(bAddr))

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
