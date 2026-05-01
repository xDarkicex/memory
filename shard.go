// Package memory — per-shard LIFO caches.
//
// Each shard owns a LIFO slot cache for local alloc/free (no atomics on
// the hot path) and a fresh cache for batch-refill slots. Deallocate always
// routes to the current goroutine's shard, keeping slots on the local CPU.
// The global FreeList underneath provides batch refills and slab management.

package memory

import (
	"sync/atomic"
	"unsafe"
)

const (
	lifoCap   = 64 // Per-shard LIFO cache capacity
	batchSize = 32 // BatchAllocate refill size
)

// shardCache is a per-shard lock-free LIFO cache using a Treiber stack.
// The slot's first 8 bytes (ptr+0) serve as the next pointer — the same
// location the global FreeList uses for its free-list chain. A slot is
// only in one list at a time, so the reuse is safe.
//
// len tracks an approximate count for capacity checking. It is updated
// after a successful CAS and may briefly overcount under contention;
// callers treat overflow as a soft signal to fall back to the global list.
type shardCache struct {
	head atomic.Uint64 // tagged pointer (tagShift=48, ptrMask lower 48 bits)
	len  atomic.Int32
}

func (c *shardCache) push(ptr unsafe.Pointer) bool {
	if c.len.Load() >= lifoCap {
		return false
	}
	for {
		old := c.head.Load()
		newTag := unpackTag(old) + 1
		atomic.StoreUint64((*uint64)(ptr), uint64(uintptr(unpackPtr(old))))
		newTagged := packTaggedPtr(ptr, newTag)
		if c.head.CompareAndSwap(old, newTagged) {
			c.len.Add(1)
			return true
		}
	}
}

func (c *shardCache) pop() unsafe.Pointer {
	for {
		old := c.head.Load()
		ptr := unpackPtr(old)
		if ptr == nil {
			return nil
		}
		newTag := unpackTag(old) + 1
		next := unsafe.Pointer(uintptr(atomic.LoadUint64((*uint64)(ptr))))
		newTagged := packTaggedPtr(next, newTag)
		if c.head.CompareAndSwap(old, newTagged) {
			n := c.len.Add(-1)
			if n < 0 {
				c.len.Store(0)
			}
			return ptr
		}
	}
}

// freshCache holds slots from BatchAllocate that are already accounted
// (slotGen set, allocated incremented). Popping from freshCache does not
// need activateSlot — just setHomeShard and return.
//
// Uses the same Treiber stack layout as shardCache.
type freshCache struct {
	head atomic.Uint64 // tagged pointer
	len  atomic.Int32
}

func (c *freshCache) push(ptr unsafe.Pointer) bool {
	if c.len.Load() >= batchSize {
		return false
	}
	for {
		old := c.head.Load()
		newTag := unpackTag(old) + 1
		atomic.StoreUint64((*uint64)(ptr), uint64(uintptr(unpackPtr(old))))
		newTagged := packTaggedPtr(ptr, newTag)
		if c.head.CompareAndSwap(old, newTagged) {
			c.len.Add(1)
			return true
		}
	}
}

func (c *freshCache) pop() unsafe.Pointer {
	for {
		old := c.head.Load()
		ptr := unpackPtr(old)
		if ptr == nil {
			return nil
		}
		newTag := unpackTag(old) + 1
		next := unsafe.Pointer(uintptr(atomic.LoadUint64((*uint64)(ptr))))
		newTagged := packTaggedPtr(next, newTag)
		if c.head.CompareAndSwap(old, newTagged) {
			n := c.len.Add(-1)
			if n < 0 {
				c.len.Store(0)
			}
			return ptr
		}
	}
}

// === Shard index selection ===
//
// getShard() is implemented in build-tagged files:
//   shard_procpin.go  → runtime.procPin()  (requires -tags procpin)
//   shard_hash.go     → stack-address hash (default, no build flags)
//
// Both return an int in [0, numShards).
