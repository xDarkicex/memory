// Package memory — Arena: bump-pointer allocator.
//
// Arena provides a single mmap'd region with CAS-based bump-pointer allocation.
// Best for single-producer, short-lived allocation bursts. Reset() reuses the
// backing memory; Free() releases it.
//
// Zero heap allocations after NewArena.

package memory

import (
	"fmt"
	"math"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Arena provides an off-heap memory arena with concurrent-safe bump allocation.
// Uses a CAS loop for lock-free allocation — safe for multiple concurrent producers.
// Single-producer use is the recommended usage pattern for best performance.
type Arena struct {
	offset atomic.Uint64
	data   []byte
	mmapd  bool
	align  uint64
}

// NewArena creates a new off-heap memory arena.
func NewArena(size uint64) (*Arena, error) {
	if size > math.MaxInt {
		return nil, fmt.Errorf("arena size %d exceeds addressable int range", size)
	}
	data, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, ErrMmapFailed
	}

	return &Arena{
		data:  data,
		mmapd: true,
		align: 8,
	}, nil
}

// Alloc allocates from the arena using pure CAS spin-loop.
// Returns (unsafe.Pointer(nil), ErrArenaExhausted) on failure.
func (a *Arena) Alloc(size uint64) (unsafe.Pointer, error) {
	if size == 0 {
		return unsafe.Pointer(nil), ErrInvalidSize
	}
	alignMask := a.align - 1

	// Pure CAS loop: no locks, scales perfectly
	for {
		// Guard against use-after-free
		if a.data == nil {
			return unsafe.Pointer(nil), ErrArenaExhausted
		}

		oldOffset := a.offset.Load()
		newOffset := (oldOffset + alignMask) &^ alignMask

		// Overflow protection: detect wraparound in offset computation
		if newOffset < oldOffset {
			return unsafe.Pointer(nil), ErrArenaExhausted
		}

		// Check allocation would exceed arena bounds
		if newOffset+size < newOffset || newOffset+size > uint64(len(a.data)) {
			return unsafe.Pointer(nil), ErrArenaExhausted
		}

		if a.offset.CompareAndSwap(oldOffset, newOffset+size) {
			ptr := unsafe.Add(unsafe.Pointer(&a.data[0]), uintptr(newOffset))
			return ptr, nil
		}
		// CAS failed: retry with fresh offset
	}
}

// Free releases arena memory. This is a destructor, not a reset.
// After Free, the arena is invalid and must not be used.
func (a *Arena) Free() error {
	if a.mmapd && len(a.data) > 0 {
		if err := unix.Munmap(a.data); err != nil {
			return err
		}
		a.data = nil // Prevent use-after-free
	}
	a.offset.Store(0)
	return nil
}

// Reset resets the arena offset to allow reuse without remapping.
// Unlike Free(), this preserves the mmap'd memory backing.
//
// WARNING: Arena is single-producer only. Calling Reset() while another
// goroutine calls Alloc() on the same arena causes overlapping allocations.
// Caller must ensure single-threaded access or use Free() + NewArena().
func (a *Arena) Reset() {
	a.offset.Store(0)
}

// Remaining returns the remaining capacity in bytes.
func (a *Arena) Remaining() uint64 {
	return uint64(len(a.data)) - a.offset.Load()
}
