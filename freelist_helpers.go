// Package memory — generic helpers for typed FreeList allocation.
//
// FreeList slots include 12 bytes of intrusive metadata at the head of each
// slot (next pointer + struct index). These helpers hide that offset so
// callers work with *T directly — no unsafe, no manual offset arithmetic.
//
// Slot layout (see pushFree):
//
//	[0:8]  next pointer  (uint64, Treiber stack link)
//	[8:12] packed meta   (uint32: structIdx | homeShard<<24)
//	[12:]  user data     ← *T points here

package memory

import "unsafe"

// metaOffset is the number of bytes of intrusive slot metadata before user
// data. It is the gap between the slot base pointer (Allocate return value)
// and where the typed user data begins.
const metaOffset = 12

// FreeListAlloc allocates a single slot from fl and returns a typed pointer
// to the user-data region. It is the typed equivalent of fl.Allocate().
//
// Panics if sizeof(T)+12 exceeds SlotSize — the check uses unsafe.Sizeof,
// a compile-time constant, so the branch is predictable and negligible.
//
// The returned *T points into off-heap mmap memory invisible to the Go GC.
// Free with FreeListDealloc; letting it escape without freeing leaks off-heap
// memory permanently.
func FreeListAlloc[T any](fl *FreeList) (*T, error) {
	var zero T
	if uint64(unsafe.Sizeof(zero))+metaOffset > fl.SlotSize() {
		return nil, ErrSlotTooSmall
	}

	slot, err := fl.Allocate()
	if err != nil {
		return nil, err
	}

	// Skip the 12-byte metadata header. The slot is off-heap mmap memory —
	// not a Go-managed object — so GC movement rules do not apply.
	ptr := unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), metaOffset)
	return (*T)(ptr), nil
}

// FreeListDealloc returns a typed pointer previously obtained from
// FreeListAlloc back to the free list. It is the typed equivalent of
// fl.Deallocate().
//
// p must have been returned by FreeListAlloc[T] on the same fl. Passing a
// pointer to any other memory is undefined behavior and will be caught by
// the bounds check in Deallocate.
//
// After this call p is invalid — any access through p is use-after-free.
func FreeListDealloc[T any](fl *FreeList, p *T) error {
	// Back up metaOffset bytes to reach the slot header. The header is
	// contiguous with user data inside the same mmap'd slab.
	slotPtr := unsafe.Add(unsafe.Pointer(p), -metaOffset)

	// Reconstruct the []byte that Deallocate expects.
	slot := unsafe.Slice((*byte)(slotPtr), fl.SlotSize())
	return fl.Deallocate(slot)
}

// MustFreeListAlloc is like FreeListAlloc but panics on exhaustion.
// Useful in initialization paths where allocation failure is fatal.
func MustFreeListAlloc[T any](fl *FreeList) *T {
	p, err := FreeListAlloc[T](fl)
	if err != nil {
		panic(err)
	}
	return p
}

// FreeListSlotFor returns the underlying []byte slot for a typed pointer
// without deallocating it. Useful when an API requires the raw []byte but
// you obtained the pointer via FreeListAlloc.
func FreeListSlotFor[T any](fl *FreeList, p *T) []byte {
	slotPtr := unsafe.Add(unsafe.Pointer(p), -metaOffset)
	return unsafe.Slice((*byte)(slotPtr), fl.SlotSize())
}
