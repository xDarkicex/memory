package memory

import "unsafe"

// offHeapPtr calculates the raw memory address for a given bucket index.
// This relies strictly on uintptr math inside the OffHeapRegion, mathematically blinding the GC.
//
//go:nosplit
func (h *HashMap) offHeapPtr(bucketIdx uint64) unsafe.Pointer {
	// bucketIdx is constrained by (size - 1) prior to calling.
	s := h.state.Load()
	return unsafe.Pointer(uintptr(s.base) + uintptr(bucketIdx*128))
}

// nextPow2 mathematically rounds up a 64-bit integer to the nearest power of 2.
// Used for enforcing modulo-arithmetic bounds on the capacity for fast bitwise indexing.
func nextPow2(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}

// TypedMap provides a type-safe generic wrapper around HashMap for typed values.
// Keys are strictly uint64 (which must be pre-hashed or casted integers) to avoid
// reflection overhead on the hot path.
type TypedMap[V any] struct {
	raw *HashMap
}

// NewTypedMap creates a new zero-allocation concurrent TypedMap.
func NewTypedMap[V any](cfg HashMapConfig) (*TypedMap[V], error) {
	raw, err := NewHashMap(cfg)
	if err != nil {
		return nil, err
	}
	return &TypedMap[V]{raw: raw}, nil
}

// Put inserts a value wait-free into the map.
func (m *TypedMap[V]) Put(key uint64, val *V) {
	m.raw.Put(key, unsafe.Pointer(val))
}

// Get retrieves a value wait-free from the map.
func (m *TypedMap[V]) Get(key uint64) (*V, bool) {
	ptr, found := m.raw.Get(key)
	if !found {
		return nil, false
	}
	return (*V)(ptr), true
}

// Delete removes a value wait-free from the map.
func (m *TypedMap[V]) Delete(key uint64) bool {
	return m.raw.Delete(key)
}
