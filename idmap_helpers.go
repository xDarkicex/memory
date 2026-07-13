package memory

import "unsafe"

// TypedIDMap is the type-safe wrapper for IDMap. V must be allocated off heap;
// storing a Go heap pointer makes it invisible to the garbage collector.
type TypedIDMap[V any] struct {
	raw *IDMap
}

func NewTypedIDMap[V any](cfg IDMapConfig) (*TypedIDMap[V], error) {
	raw, err := NewIDMap(cfg)
	if err != nil {
		return nil, err
	}
	return &TypedIDMap[V]{raw: raw}, nil
}

func (m *TypedIDMap[V]) PutString(id string, value *V) error {
	return m.raw.PutString(id, unsafe.Pointer(value))
}

func (m *TypedIDMap[V]) PutBytes(id []byte, value *V) error {
	return m.raw.PutBytes(id, unsafe.Pointer(value))
}

func (m *TypedIDMap[V]) PutStringIfAbsent(id string, value *V) (*V, bool, error) {
	ptr, inserted, err := m.raw.PutStringIfAbsent(id, unsafe.Pointer(value))
	return (*V)(ptr), inserted, err
}

func (m *TypedIDMap[V]) PutBytesIfAbsent(id []byte, value *V) (*V, bool, error) {
	ptr, inserted, err := m.raw.PutBytesIfAbsent(id, unsafe.Pointer(value))
	return (*V)(ptr), inserted, err
}

func (m *TypedIDMap[V]) GetString(id string) (*V, bool) {
	ptr, found := m.raw.GetString(id)
	return (*V)(ptr), found
}

func (m *TypedIDMap[V]) GetBytes(id []byte) (*V, bool) {
	ptr, found := m.raw.GetBytes(id)
	return (*V)(ptr), found
}

func (m *TypedIDMap[V]) DeleteString(id string) bool { return m.raw.DeleteString(id) }

func (m *TypedIDMap[V]) DeleteBytes(id []byte) bool { return m.raw.DeleteBytes(id) }

func (m *TypedIDMap[V]) Free() error { return m.raw.Free() }
