package memory

import (
	"errors"
	"sync/atomic"
	"unsafe"
)

// HashMapConfig defines the parameters for the zero-allocation map.
// Alignment ensures buckets are properly padded for AVX-512 vector loads.
type HashMapConfig struct {
	Alignment uint64
	Capacity  uint64
}

// mapState holds one mmap generation. retired chains generations that have
// been superseded by resize but cannot be unmapped until HashMap.Free.
type mapState struct {
	base             unsafe.Pointer
	size             uint64
	mmapSize         uint64
	bucketsRemaining atomic.Uint64
	next             *mapState
	retired          atomic.Pointer[mapState]
}

const (
	bucketMigratingBit = uint64(1) << 28
	// The hopscotch mask is currently unused; reserve its first bit to mark a
	// bucket whose migration has already decremented bucketsRemaining.
	bucketMigratedBit = uint64(1) << 7
)

// HashMap is a cybernetic, zero-allocation, wait-free concurrent map backed by
// mmap'd off-heap memory. The entire bucket array lives outside the Go heap — the
// GC never scans it. The map uses CAS-based publishing with cooperative migration
// (Dr. Cliff Click's wait-free hash map design). Superseded mmap generations are
// retained until Free: a single mmap is not a valid Hyaline batch, so eagerly
// retiring it cannot protect every concurrent reader.
//
// All values stored in the map must be off-heap pointers (Arena, FreeList, Pool,
// or ShardedFreeList allocations). Go heap pointers stored in the map are invisible
// to the GC and will be collected. The race detector's checkptr instrumentation
// catches Go heap pointers at test time.
type HashMap struct {
	cfg   HashMapConfig
	state atomic.Pointer[mapState]
}

// NewHashMap creates a new lock-free map mapped directly to physical RAM.
func NewHashMap(cfg HashMapConfig) (*HashMap, error) {
	if cfg.Alignment == 0 {
		cfg.Alignment = 128 // Default to Apple Silicon / AVX-512 optimal alignment
	}
	if cfg.Alignment&(cfg.Alignment-1) != 0 {
		return nil, errors.New("Alignment must be a power of 2")
	}

	capacity := cfg.Capacity
	if capacity < 8 {
		capacity = 8
	}
	// Round up to nearest power of 2 for fast modulo
	capacity--
	capacity |= capacity >> 1
	capacity |= capacity >> 2
	capacity |= capacity >> 4
	capacity |= capacity >> 8
	capacity |= capacity >> 16
	capacity |= capacity >> 32
	capacity++

	// 7 slots per bucket, so bucket count is Capacity / 7
	bucketCount := (capacity + 6) / 7

	// Force bucket count to be a power of 2 for fast & (size - 1)
	bucketCount--
	bucketCount |= bucketCount >> 1
	bucketCount |= bucketCount >> 2
	bucketCount |= bucketCount >> 4
	bucketCount |= bucketCount >> 8
	bucketCount |= bucketCount >> 16
	bucketCount |= bucketCount >> 32
	bucketCount++

	// 128 bytes per bucket plus a 128-byte prefix so the bucket base remains
	// 128-byte aligned. The prefix is retained for stable mapping layout.
	allocSize := bucketCount*128 + 128

	addr, err := mmapRawAnonymous(allocSize)
	if err != nil {
		return nil, err
	}

	h := &HashMap{
		cfg: cfg,
	}

	h.state.Store(&mapState{
		base:     unsafe.Pointer(addr + 128),
		size:     bucketCount,
		mmapSize: allocSize,
	})

	return h, nil
}

// Free releases every mmap generation owned by the map. It must not race with
// Put, PutIfAbsent, Get, Delete, or Range; after Free the map is invalid.
func (h *HashMap) Free() error {
	if h == nil {
		return nil
	}
	state := h.state.Swap(nil)
	var firstErr error
	// A quiescent caller can destroy a map while migration is installed but
	// before promotion. That successor is not part of the retired chain yet.
	if state != nil && state.next != nil {
		if err := munmapRaw(uintptr(state.next.base)-128, state.next.mmapSize); err != nil {
			firstErr = err
		}
	}
	for state != nil {
		if err := munmapRaw(uintptr(state.base)-128, state.mmapSize); err != nil && firstErr == nil {
			firstErr = err
		}
		state = state.retired.Load()
	}
	return firstErr
}

// Bucket perfectly aligns to a 128-byte cache line boundary.
// It stores 7 keys and values.
type Bucket struct {
	Metadata atomic.Uint64
	Keys     [7]atomic.Uint64
	Vals     [7]atomic.Uintptr
	_        uint64 // Padding to exactly 128 bytes
}

// ErrNeedsResize is an internal signal that a bucket is full and a resize is needed.
var ErrNeedsResize = errors.New("hashmap needs resize")
