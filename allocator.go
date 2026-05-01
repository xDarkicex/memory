// Package memory provides low-level memory management primitives for
// native systems environments. Implements off-heap allocation via mmap
// for complete GC isolation and deterministic runtime behavior.
package memory

import (
	"errors"
	"os"
)

// Error sentinels — every failure mode has a pre-allocated error value so
// callers can use errors.Is without allocating.
var (
	ErrPoolExhausted          = errors.New("pool exhausted: cannot expand under memory pressure")
	ErrInvalidSize            = errors.New("invalid allocation size: must be greater than 0")
	ErrArenaExhausted         = errors.New("arena exhausted: insufficient space for allocation")
	ErrMmapFailed             = errors.New("mmap allocation failed: system limit or OOM")
	ErrPoolFreed              = errors.New("pool has been freed: no further allocations allowed")
	ErrFreelistFreed          = errors.New("freelist has been freed: no further allocations allowed")
	ErrArenaCapacityExceeded  = errors.New("arena slice capacity exceeded")
	ErrSlotTooSmall           = errors.New("slot too small: sizeof(T)+12 exceeds SlotSize")
)

// PageSize is the actual system page size obtained via OS syscall.
var PageSize = os.Getpagesize()

// HugepageSize is the huge page size on Linux (detected at init from /proc/meminfo).
// Used for validation when UseHugePages is enabled.
// This value is Linux-specific; Darwin has no working huge page support
// and UseHugePages is ignored there.
// NOTE: Other Linux architectures (e.g. arm64) may use different huge page sizes.
var HugepageSize uint64 = 2 * 1024 * 1024

// AllocatorConfig holds memory allocator settings.
type AllocatorConfig struct {
	// PoolSize is the maximum pool size in bytes (hard limit on mmap'd memory).
	PoolSize uint64
	// UseHugePages attempts explicit huge page allocation via MAP_HUGETLB (Linux only).
	// Falls back to regular mmap if huge pages are unavailable or not configured.
	// On Darwin: UseHugePages is silently ignored — macOS has no working huge page
	// support for user code; regular mmap is always used.
	// When enabled, SlabSize should be a multiple of HugepageSize for best results.
	UseHugePages bool
	// Prealloc enables pre-allocation of memory pools at creation time.
	Prealloc bool
	// SlabCount is the initial number of slab descriptors to pre-allocate.
	SlabCount int
	// SlabSize is the size of each slab (default 1MB, should be >= HugepageSize for hugepages).
	SlabSize uint64
}

// DefaultConfig returns the default allocator configuration.
func DefaultConfig() AllocatorConfig {
	return AllocatorConfig{
		PoolSize:     64 * 1024 * 1024, // 64MB pool limit
		UseHugePages: false,
		Prealloc:     false,
		SlabCount:    16,
		SlabSize:     1024 * 1024, // 1MB slabs for throughput
	}
}
