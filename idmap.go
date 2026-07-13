package memory

import (
	"errors"
	"math"
	"math/bits"
	"runtime"
	"sync/atomic"
	"unsafe"
)

const (
	idMapSlotsPerBucket = uint64(4)
	idMapBucketBytes    = uintptr(128)

	idMapOccupiedShift = uint64(0)
	idMapReservedShift = uint64(4)
	idMapForwardShift  = uint64(8)
	idMapTombShift     = uint64(12)
	idMapMigratingBit  = uint64(1) << 16
	idMapMigratedBit   = uint64(1) << 17
	idMapFingerprintAt = uint64(24)
	idMapSlotMask      = uint64(0x0f)
)

var (
	ErrIDMapFreed            = errors.New("id map has been freed")
	ErrIDMapKeyTooLarge      = errors.New("id map key exceeds uint32 length")
	ErrIDMapInvalidAlignment = errors.New("id map alignment must be a power of two no larger than 128 bytes")
)

// IDMapConfig controls an IDMap's table and copied-key storage.
// KeyBytes is a hard bound: keys are append-only until Free, including keys
// deleted while concurrent readers may still hold references to their bytes.
type IDMapConfig struct {
	Capacity  uint64
	KeyBytes  uint64
	Alignment uint64
}

// IDBucket occupies exactly two 64-byte cache lines. Metadata publishes slot
// state only after hash, key, length, and value have been initialized.
type IDBucket struct {
	Metadata atomic.Uint64
	Hashes   [4]atomic.Uint64
	KeyPtrs  [4]atomic.Uintptr
	Vals     [4]atomic.Uintptr
	KeyLens  [4]atomic.Uint32
	_        [8]byte
}

type idMapState struct {
	base             unsafe.Pointer
	size             uint64
	mmapSize         uint64
	bucketsRemaining atomic.Uint64
	next             *idMapState
	retired          *idMapState
}

// IDMap is a collision-correct concurrent map from string/byte IDs to off-heap
// pointers. Bucket arrays and copied ID bytes are mmap-backed and invisible to
// the Go GC. Values must also point to off-heap memory.
//
// Resized bucket generations remain mapped until Free. This gives readers
// stable addresses without hazard-pointer traffic on Get; geometric growth
// bounds retired bucket memory below the current generation's size.
type IDMap struct {
	state  atomic.Pointer[idMapState]
	keys   *Arena
	closed atomic.Bool
}

type idMapKey struct {
	ptr  unsafe.Pointer
	hash uint64
	len  uint32
}

func NewIDMap(cfg IDMapConfig) (*IDMap, error) {
	if cfg.Alignment == 0 {
		cfg.Alignment = 128
	}
	if cfg.Alignment&(cfg.Alignment-1) != 0 {
		return nil, ErrIDMapInvalidAlignment
	}
	if cfg.Alignment > uint64(idMapBucketBytes) {
		return nil, ErrIDMapInvalidAlignment
	}
	if cfg.KeyBytes == 0 {
		if cfg.Capacity > math.MaxUint64/64 {
			return nil, ErrInvalidSize
		}
		cfg.KeyBytes = cfg.Capacity * 64
		if cfg.KeyBytes < 4096 {
			cfg.KeyBytes = 4096
		}
	}

	bucketCount := idMapBucketCount(cfg.Capacity)
	state, err := allocateIDMapState(bucketCount)
	if err != nil {
		return nil, err
	}
	keys, err := NewArena(cfg.KeyBytes, 8)
	if err != nil {
		_ = munmapRaw(uintptr(state.base), state.mmapSize)
		return nil, err
	}
	m := &IDMap{keys: keys}
	m.state.Store(state)
	return m, nil
}

func idMapBucketCount(capacity uint64) uint64 {
	if capacity < 8 {
		capacity = 8
	}
	buckets := (capacity + idMapSlotsPerBucket - 1) / idMapSlotsPerBucket
	return nextPow2(buckets)
}

func allocateIDMapState(bucketCount uint64) (*idMapState, error) {
	if bucketCount == 0 || bucketCount > math.MaxUint64/uint64(idMapBucketBytes) {
		return nil, ErrInvalidSize
	}
	size := bucketCount * uint64(idMapBucketBytes)
	addr, err := mmapRawAnonymous(size)
	if err != nil {
		return nil, err
	}
	return &idMapState{base: unsafe.Pointer(addr), size: bucketCount, mmapSize: size}, nil
}

func (m *IDMap) PutString(id string, value unsafe.Pointer) error {
	key, err := idMapStringKey(id)
	if err != nil {
		return err
	}
	err = m.put(key, value, false, nil)
	runtime.KeepAlive(id)
	return err
}

func (m *IDMap) PutBytes(id []byte, value unsafe.Pointer) error {
	key, err := idMapBytesKey(id)
	if err != nil {
		return err
	}
	err = m.put(key, value, false, nil)
	runtime.KeepAlive(id)
	return err
}

func (m *IDMap) PutStringIfAbsent(id string, value unsafe.Pointer) (unsafe.Pointer, bool, error) {
	key, err := idMapStringKey(id)
	if err != nil {
		return nil, false, err
	}
	var result idMapPutResult
	err = m.put(key, value, true, &result)
	runtime.KeepAlive(id)
	return result.value, result.inserted, err
}

func (m *IDMap) PutBytesIfAbsent(id []byte, value unsafe.Pointer) (unsafe.Pointer, bool, error) {
	key, err := idMapBytesKey(id)
	if err != nil {
		return nil, false, err
	}
	var result idMapPutResult
	err = m.put(key, value, true, &result)
	runtime.KeepAlive(id)
	return result.value, result.inserted, err
}

func (m *IDMap) GetString(id string) (unsafe.Pointer, bool) {
	key, err := idMapStringKey(id)
	if err != nil || m.closed.Load() {
		return nil, false
	}
	value, found := m.get(key)
	runtime.KeepAlive(id)
	return value, found
}

func (m *IDMap) GetBytes(id []byte) (unsafe.Pointer, bool) {
	key, err := idMapBytesKey(id)
	if err != nil || m.closed.Load() {
		return nil, false
	}
	value, found := m.get(key)
	runtime.KeepAlive(id)
	return value, found
}

func (m *IDMap) DeleteString(id string) bool {
	key, err := idMapStringKey(id)
	if err != nil || m.closed.Load() {
		return false
	}
	deleted := m.delete(key)
	runtime.KeepAlive(id)
	return deleted
}

func (m *IDMap) DeleteBytes(id []byte) bool {
	key, err := idMapBytesKey(id)
	if err != nil || m.closed.Load() {
		return false
	}
	deleted := m.delete(key)
	runtime.KeepAlive(id)
	return deleted
}

// Free releases all key and bucket mappings. It must not race with map ops.
func (m *IDMap) Free() error {
	if m == nil || !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	for {
		s := m.state.Load()
		if s == nil || s.next == nil {
			break
		}
		m.helpMigrateAll(s)
	}
	s := m.state.Swap(nil)
	var firstErr error
	for s != nil {
		if s.base != nil {
			if err := munmapRaw(uintptr(s.base), s.mmapSize); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		s = s.retired
	}
	if m.keys != nil {
		if err := m.keys.Free(); err != nil && firstErr == nil {
			firstErr = err
		}
		m.keys = nil
	}
	return firstErr
}

func idMapStringKey(id string) (idMapKey, error) {
	if uint64(len(id)) > math.MaxUint32 {
		return idMapKey{}, ErrIDMapKeyTooLarge
	}
	var ptr unsafe.Pointer
	if len(id) != 0 {
		ptr = unsafe.Pointer(unsafe.StringData(id))
	}
	return idMapKey{ptr: ptr, len: uint32(len(id)), hash: hashID(ptr, uint32(len(id)))}, nil
}

func idMapBytesKey(id []byte) (idMapKey, error) {
	if uint64(len(id)) > math.MaxUint32 {
		return idMapKey{}, ErrIDMapKeyTooLarge
	}
	var ptr unsafe.Pointer
	if len(id) != 0 {
		ptr = unsafe.Pointer(unsafe.SliceData(id))
	}
	return idMapKey{ptr: ptr, len: uint32(len(id)), hash: hashID(ptr, uint32(len(id)))}, nil
}

// hashID is a word-at-a-time non-cryptographic hash with a SplitMix avalanche.
// It reads unaligned words, supported by every target architecture in this module.
//
//go:nosplit
func hashID(ptr unsafe.Pointer, n uint32) uint64 {
	h := uint64(n)*0x9e3779b97f4a7c15 ^ 0x243f6a8885a308d3
	offset := uintptr(0)
	remaining := n
	for remaining >= 8 {
		word := *(*uint64)(unsafe.Add(ptr, offset))
		h ^= SplitMix64(word + uint64(offset))
		h = (h << 27) | (h >> 37)
		h *= 0x94d049bb133111eb
		offset += 8
		remaining -= 8
	}
	var tail uint64
	for i := uint32(0); i < remaining; i++ {
		tail |= uint64(*(*byte)(unsafe.Add(ptr, offset+uintptr(i)))) << (i * 8)
	}
	return SplitMix64(h ^ tail)
}

//go:nosplit
func idMapKeysEqual(stored uintptr, storedLen uint32, key idMapKey) bool {
	if storedLen != key.len {
		return false
	}
	left := unsafe.Pointer(stored)
	right := key.ptr
	offset := uintptr(0)
	remaining := storedLen
	for remaining >= 8 {
		if *(*uint64)(unsafe.Add(left, offset)) != *(*uint64)(unsafe.Add(right, offset)) {
			return false
		}
		offset += 8
		remaining -= 8
	}
	for i := uint32(0); i < remaining; i++ {
		if *(*byte)(unsafe.Add(left, offset+uintptr(i))) != *(*byte)(unsafe.Add(right, offset+uintptr(i))) {
			return false
		}
	}
	return true
}

func idMapBucket(s *idMapState, index uint64) *IDBucket {
	return (*IDBucket)(unsafe.Add(s.base, uintptr(index)*idMapBucketBytes))
}

func idMapFingerprint(hash uint64) uint64 { return (hash >> 56) & 0xff }

func idMapFingerprintMask(meta, fingerprint uint64) uint64 {
	fingerprints := uint32(meta >> idMapFingerprintAt)
	target := uint32(fingerprint) * 0x01010101
	x := fingerprints ^ target
	zeroBytes := (x - 0x01010101) &^ x & 0x80808080
	match := ((zeroBytes >> 7) & 0x01) |
		((zeroBytes >> 14) & 0x02) |
		((zeroBytes >> 21) & 0x04) |
		((zeroBytes >> 28) & 0x08)
	return uint64(match) & ((meta >> idMapOccupiedShift) & idMapSlotMask)
}

func idMapFirst(mask uint64) uint32 { return uint32(bits.TrailingZeros64(mask)) }
