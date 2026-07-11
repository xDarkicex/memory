// Package memory — generic helpers for typed ShardedFreeList allocation.
//
// ShardedFreeList slots have a 16-byte gap (metaOffset) at the head before typed user
// data begins. The 16-byte alignment satisfies ARM64 checkptr requirements
// for Go types containing pointers (slice headers, interfaces).
// These helpers hide that offset so callers work with *T directly
// — no unsafe, no manual offset arithmetic.
//
// Slot layout (see pushFree):
//
//	[0:8]   next pointer  (uint64, Treiber stack link)
//	[8:16]  padding       (alignment to 16 bytes for ARM64)
//	[16:]   user data     ← *T points here (properly aligned)

package memory

import "unsafe"

// ShardedFreeListAlloc allocates a single slot from sfl and returns a typed pointer
// to the user-data region. It is the typed equivalent of sfl.Allocate().
//
// Panics if sizeof(T)+12 exceeds SlotSize — the check uses unsafe.Sizeof,
// a compile-time constant, so the branch is predictable and negligible.
//
// The returned *T points into off-heap mmap memory invisible to the Go GC.
// Free with ShardedFreeListDealloc or ShardedFreeListRetire.
func ShardedFreeListAlloc[T any](sfl *ShardedFreeList) (*T, error) {
	var typed *T
	if uint64(unsafe.Sizeof(*typed))+metaOffset > sfl.cfg.SlotSize {
		return nil, ErrSlotTooSmall
	}

	slot, err := sfl.Allocate()
	if err != nil {
		return nil, err
	}

	// Skip the 16-byte metadata header. The slot is off-heap mmap memory —
	// not a Go-managed object — so GC movement rules do not apply.
	ptr := unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), metaOffset)
	return (*T)(ptr), nil
}

// ShardedFreeListDealloc returns a typed pointer previously obtained from
// ShardedFreeListAlloc back to the free list immediately (bypassing SMR).
// It is the typed equivalent of sfl.Deallocate().
//
// p must have been returned by ShardedFreeListAlloc[T] on the same sfl.
// After this call p is invalid — any access through p is use-after-free.
func ShardedFreeListDealloc[T any](sfl *ShardedFreeList, p *T) error {
	slotPtr := unsafe.Add(unsafe.Pointer(p), -metaOffset)
	slot := unsafe.Slice((*byte)(slotPtr), sfl.cfg.SlotSize)
	return sfl.Deallocate(slot)
}

// ShardedFreeListRetire schedules a typed pointer previously obtained from
// ShardedFreeListAlloc for safe reclamation using Hyaline SMR.
// It is the typed equivalent of sfl.Retire().
//
// p must have been returned by ShardedFreeListAlloc[T] on the same sfl.
func ShardedFreeListRetire[T any](sfl *ShardedFreeList, p *T) error {
	slotPtr := unsafe.Add(unsafe.Pointer(p), -metaOffset)
	slot := unsafe.Slice((*byte)(slotPtr), sfl.cfg.SlotSize)
	return sfl.Retire(slot)
}

// MustShardedFreeListAlloc is like ShardedFreeListAlloc but panics on exhaustion.
// Useful in initialization paths where allocation failure is fatal.
func MustShardedFreeListAlloc[T any](sfl *ShardedFreeList) *T {
	p, err := ShardedFreeListAlloc[T](sfl)
	if err != nil {
		panic(err)
	}
	return p
}

// ShardedFreeListSlotFor returns the underlying []byte slot for a typed pointer
// without deallocating it. Useful when an API requires the raw []byte but
// you obtained the pointer via ShardedFreeListAlloc.
func ShardedFreeListSlotFor[T any](sfl *ShardedFreeList, p *T) []byte {
	slotPtr := unsafe.Add(unsafe.Pointer(p), -metaOffset)
	return unsafe.Slice((*byte)(slotPtr), sfl.cfg.SlotSize)
}
