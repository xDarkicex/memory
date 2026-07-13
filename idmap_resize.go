package memory

import "unsafe"

func (m *IDMap) triggerResize(s *idMapState) error {
	if s.next != nil {
		return nil
	}
	next, err := allocateIDMapState(s.size * 2)
	if err != nil {
		return err
	}
	wrapper := &idMapState{
		base:     s.base,
		size:     s.size,
		mmapSize: s.mmapSize,
		next:     next,
		retired:  s.retired,
	}
	wrapper.bucketsRemaining.Store(s.size)
	next.retired = wrapper
	if !m.state.CompareAndSwap(s, wrapper) {
		_ = munmapRaw(uintptr(next.base), next.mmapSize)
	}
	return nil
}

func (m *IDMap) helpMigrateAll(s *idMapState) {
	if s.next == nil {
		return
	}
	for bucket := uint64(0); bucket < s.size; bucket++ {
		if s.bucketsRemaining.Load() == 0 {
			return
		}
		m.helpMigrate(s, bucket)
	}
}

func (m *IDMap) helpMigrate(s *idMapState, bucketIndex uint64) {
	if s.next == nil {
		return
	}
	bucket := idMapBucket(s, bucketIndex)
	for {
		meta := bucket.Metadata.Load()
		if meta&idMapMigratedBit != 0 {
			return
		}
		if (meta>>idMapReservedShift)&idMapSlotMask != 0 {
			Spin()
			continue
		}
		if meta&idMapMigratingBit == 0 {
			if !bucket.Metadata.CompareAndSwap(meta, meta|idMapMigratingBit) {
				continue
			}
			meta |= idMapMigratingBit
		}

		occupied := (meta >> idMapOccupiedShift) & idMapSlotMask
		forwarded := (meta >> idMapForwardShift) & idMapSlotMask
		if occupied == forwarded {
			finished := (meta | idMapMigratedBit) &^ idMapMigratingBit
			if bucket.Metadata.CompareAndSwap(meta, finished) {
				if s.bucketsRemaining.Add(^uint64(0)) == 0 {
					m.state.CompareAndSwap(s, s.next)
				}
				return
			}
			continue
		}

		slot := idMapFirst(occupied &^ forwarded)
		key := idMapKey{
			ptr:  unsafe.Pointer(bucket.KeyPtrs[slot].Load()),
			len:  bucket.KeyLens[slot].Load(),
			hash: bucket.Hashes[slot].Load(),
		}
		value := unsafe.Pointer(bucket.Vals[slot].Load())
		m.migrateEntry(s, bucket, slot, key, value)
		for {
			current := bucket.Metadata.Load()
			forwardBit := uint64(1) << (idMapForwardShift + uint64(slot))
			if current&forwardBit != 0 || bucket.Metadata.CompareAndSwap(current, current|forwardBit) {
				break
			}
		}
	}
}

// migrateEntry follows the currently writable generation. A delayed helper
// may outlive one or more resizes, so writing unconditionally to source.next
// can livelock on a generation that is already migrating. Put-if-absent also
// prevents a stale helper from replacing a value updated in a newer generation.
func (m *IDMap) migrateEntry(source *idMapState, bucket *IDBucket, slot uint32, key idMapKey, value unsafe.Pointer) {
	forwardBit := uint64(1) << (idMapForwardShift + uint64(slot))
	for {
		if bucket.Metadata.Load()&forwardBit != 0 {
			return
		}

		target := source.next
		current := m.state.Load()
		if current == nil {
			return
		}
		if current != source {
			target = current
			if current.next != nil {
				target = current.next
			}
		}

		_, _, err := m.putInner(target, key, value, true, false)
		if err == nil {
			return
		}
		Spin()
	}
}
