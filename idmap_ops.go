package memory

import (
	"errors"
	"unsafe"
)

var errIDMapRetry = errors.New("id map retry")

type idMapPutResult struct {
	value    unsafe.Pointer
	inserted bool
}

func (m *IDMap) put(key idMapKey, value unsafe.Pointer, onlyAbsent bool, result *idMapPutResult) error {
	for {
		if m.closed.Load() {
			return ErrIDMapFreed
		}
		s := m.state.Load()
		if s == nil {
			return ErrIDMapFreed
		}
		if s.next != nil {
			m.helpMigrateAll(s)
			continue
		}
		existing, inserted, err := m.putInner(s, key, value, onlyAbsent, true)
		if err == nil {
			if result != nil {
				result.value = existing
				result.inserted = inserted
			}
			return nil
		}
		if err == errIDMapRetry {
			Spin()
			continue
		}
		if err != ErrNeedsResize {
			return err
		}
		if current := m.state.Load(); current == s && current.next == nil {
			if err := m.triggerResize(s); err != nil {
				return err
			}
		}
	}
}

func (m *IDMap) putInner(s *idMapState, key idMapKey, value unsafe.Pointer, onlyAbsent, copyKey bool) (unsafe.Pointer, bool, error) {
	fingerprint := idMapFingerprint(key.hash)
	start := key.hash & (s.size - 1)
	return m.putInnerReserved(s, key, value, onlyAbsent, copyKey, fingerprint, start)
}

func (m *IDMap) putInnerReserved(s *idMapState, key idMapKey, value unsafe.Pointer, onlyAbsent, copyKey bool, fingerprint, start uint64) (unsafe.Pointer, bool, error) {
	var candidate *IDBucket
	var candidateSlot uint32
	var candidateTomb bool

	for probe := uint64(0); probe < s.size; probe++ {
		bucket := idMapBucket(s, (start+probe)&(s.size-1))
		meta := bucket.Metadata.Load()
		if meta&(idMapMigratingBit|idMapMigratedBit) != 0 {
			return nil, false, ErrNeedsResize
		}
		if (meta>>idMapReservedShift)&idMapSlotMask != 0 {
			return nil, false, errIDMapRetry
		}

		matches := idMapFingerprintMask(meta, fingerprint)
		for matches != 0 {
			slot := idMapFirst(matches)
			if bucket.Hashes[slot].Load() == key.hash && idMapKeysEqual(bucket.KeyPtrs[slot].Load(), bucket.KeyLens[slot].Load(), key) {
				existing := unsafe.Pointer(bucket.Vals[slot].Load())
				if onlyAbsent {
					return existing, false, nil
				}
				reserveBit := uint64(1) << (idMapReservedShift + uint64(slot))
				if !bucket.Metadata.CompareAndSwap(meta, meta|reserveBit) {
					return nil, false, errIDMapRetry
				}
				bucket.Vals[slot].Store(uintptr(value))
				for {
					current := bucket.Metadata.Load()
					if current&(idMapMigratingBit|idMapMigratedBit) != 0 ||
						current&(uint64(1)<<(idMapOccupiedShift+uint64(slot))) == 0 {
						idMapClearReservation(bucket, reserveBit)
						return nil, false, errIDMapRetry
					}
					if bucket.Metadata.CompareAndSwap(current, current&^reserveBit) {
						break
					}
				}
				if bucket.Hashes[slot].Load() == key.hash {
					return value, false, nil
				}
				return nil, false, errIDMapRetry
			}
			matches &= matches - 1
		}

		occupied := (meta >> idMapOccupiedShift) & idMapSlotMask
		reserved := (meta >> idMapReservedShift) & idMapSlotMask
		tombs := (meta >> idMapTombShift) & idMapSlotMask
		availableTomb := tombs &^ occupied &^ reserved
		if candidate == nil && availableTomb != 0 {
			candidate = bucket
			candidateSlot = idMapFirst(availableTomb)
			candidateTomb = true
		}
		trueEmpty := idMapSlotMask &^ (occupied | reserved | tombs)
		if trueEmpty == 0 {
			continue
		}
		if candidate == nil {
			candidate = bucket
			candidateSlot = idMapFirst(trueEmpty)
			candidateTomb = false
		}
		return m.reserveAndPublish(candidate, candidateSlot, candidateTomb, key, value, copyKey)
	}

	if candidate != nil {
		return m.reserveAndPublish(candidate, candidateSlot, candidateTomb, key, value, copyKey)
	}
	return nil, false, ErrNeedsResize
}

func idMapClearReservation(bucket *IDBucket, reserveBit uint64) {
	for {
		meta := bucket.Metadata.Load()
		if meta&reserveBit == 0 || bucket.Metadata.CompareAndSwap(meta, meta&^reserveBit) {
			return
		}
	}
}

func (m *IDMap) reserveAndPublish(bucket *IDBucket, slot uint32, tomb bool, key idMapKey, value unsafe.Pointer, copyKey bool) (unsafe.Pointer, bool, error) {
	reserveBit := uint64(1) << (idMapReservedShift + uint64(slot))
	occupiedBit := uint64(1) << (idMapOccupiedShift + uint64(slot))
	tombBit := uint64(1) << (idMapTombShift + uint64(slot))

	for {
		meta := bucket.Metadata.Load()
		if meta&(idMapMigratingBit|idMapMigratedBit) != 0 || meta&reserveBit != 0 || meta&occupiedBit != 0 {
			return nil, false, errIDMapRetry
		}
		if tomb && meta&tombBit == 0 {
			return nil, false, errIDMapRetry
		}
		if !tomb && meta&tombBit != 0 {
			return nil, false, errIDMapRetry
		}
		if bucket.Metadata.CompareAndSwap(meta, meta|reserveBit) {
			break
		}
	}

	storedKey := key.ptr
	if copyKey && key.len != 0 {
		var err error
		storedKey, err = m.keys.Alloc(uint64(key.len))
		if err != nil {
			m.releaseReservation(bucket, slot, tomb)
			return nil, false, err
		}
		copy(unsafe.Slice((*byte)(storedKey), int(key.len)), unsafe.Slice((*byte)(key.ptr), int(key.len)))
	}

	bucket.Hashes[slot].Store(key.hash)
	bucket.KeyPtrs[slot].Store(uintptr(storedKey))
	bucket.KeyLens[slot].Store(key.len)
	bucket.Vals[slot].Store(uintptr(value))

	fingerprintShift := idMapFingerprintAt + uint64(slot)*8
	for {
		meta := bucket.Metadata.Load()
		if meta&reserveBit == 0 {
			return nil, false, errIDMapRetry
		}
		published := meta | occupiedBit
		published &^= reserveBit | tombBit | (uint64(0xff) << fingerprintShift)
		published |= idMapFingerprint(key.hash) << fingerprintShift
		if bucket.Metadata.CompareAndSwap(meta, published) {
			return value, true, nil
		}
	}
}

func (m *IDMap) releaseReservation(bucket *IDBucket, slot uint32, tomb bool) {
	reserveBit := uint64(1) << (idMapReservedShift + uint64(slot))
	tombBit := uint64(1) << (idMapTombShift + uint64(slot))
	for {
		meta := bucket.Metadata.Load()
		next := meta &^ reserveBit
		if tomb {
			next |= tombBit
		}
		if bucket.Metadata.CompareAndSwap(meta, next) {
			return
		}
	}
}

func (m *IDMap) get(key idMapKey) (unsafe.Pointer, bool) {
	for {
		s := m.state.Load()
		if s == nil {
			return nil, false
		}
		if s.next != nil {
			m.helpMigrateAll(s)
			continue
		}
		return m.getInner(s, key)
	}
}

func (m *IDMap) getInner(s *idMapState, key idMapKey) (unsafe.Pointer, bool) {
	fingerprint := idMapFingerprint(key.hash)
	start := key.hash & (s.size - 1)
	for probe := uint64(0); probe < s.size; probe++ {
		bucket := idMapBucket(s, (start+probe)&(s.size-1))
		meta := bucket.Metadata.Load()
		matches := idMapFingerprintMask(meta, fingerprint)
		for matches != 0 {
			slot := idMapFirst(matches)
			if bucket.Hashes[slot].Load() == key.hash && idMapKeysEqual(bucket.KeyPtrs[slot].Load(), bucket.KeyLens[slot].Load(), key) {
				return unsafe.Pointer(bucket.Vals[slot].Load()), true
			}
			matches &= matches - 1
		}
		occupied := (meta >> idMapOccupiedShift) & idMapSlotMask
		reserved := (meta >> idMapReservedShift) & idMapSlotMask
		tombs := (meta >> idMapTombShift) & idMapSlotMask
		if occupied|reserved|tombs != idMapSlotMask {
			return nil, false
		}
	}
	return nil, false
}

func (m *IDMap) delete(key idMapKey) bool {
	for {
		s := m.state.Load()
		if s == nil {
			return false
		}
		if s.next != nil {
			m.helpMigrateAll(s)
			continue
		}
		deleted, retry := m.deleteInner(s, key)
		if retry {
			Spin()
			continue
		}
		return deleted
	}
}

func (m *IDMap) deleteInner(s *idMapState, key idMapKey) (bool, bool) {
	fingerprint := idMapFingerprint(key.hash)
	start := key.hash & (s.size - 1)
	return m.deleteInnerReserved(s, key, fingerprint, start)
}

func (m *IDMap) deleteInnerReserved(s *idMapState, key idMapKey, fingerprint, start uint64) (bool, bool) {
	for probe := uint64(0); probe < s.size; probe++ {
		bucket := idMapBucket(s, (start+probe)&(s.size-1))
		for {
			meta := bucket.Metadata.Load()
			if meta&(idMapMigratingBit|idMapMigratedBit) != 0 || (meta>>idMapReservedShift)&idMapSlotMask != 0 {
				return false, true
			}
			matches := idMapFingerprintMask(meta, fingerprint)
			var matched bool
			for matches != 0 {
				slot := idMapFirst(matches)
				if bucket.Hashes[slot].Load() == key.hash && idMapKeysEqual(bucket.KeyPtrs[slot].Load(), bucket.KeyLens[slot].Load(), key) {
					matched = true
					occupiedBit := uint64(1) << (idMapOccupiedShift + uint64(slot))
					reserveBit := uint64(1) << (idMapReservedShift + uint64(slot))
					tombBit := uint64(1) << (idMapTombShift + uint64(slot))
					if !bucket.Metadata.CompareAndSwap(meta, meta|reserveBit) {
						break
					}
					reserved := meta | reserveBit
					if bucket.Metadata.CompareAndSwap(reserved, ((reserved&^occupiedBit)&^reserveBit)|tombBit) {
						return true, false
					}
					idMapClearReservation(bucket, reserveBit)
					break
				}
				matches &= matches - 1
			}
			if matched {
				continue
			}
			occupied := (meta >> idMapOccupiedShift) & idMapSlotMask
			reserved := (meta >> idMapReservedShift) & idMapSlotMask
			tombs := (meta >> idMapTombShift) & idMapSlotMask
			if occupied|reserved|tombs != idMapSlotMask {
				return false, false
			}
			break
		}
	}
	return false, false
}
