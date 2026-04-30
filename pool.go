// Package memory — Pool: concurrent slab allocator.
//
// Pool serves variable-size off-heap allocations from mmap'd slabs via
// lock-free CAS on the hot path. Small allocations (≤ SlabSize) use
// per-slab CAS; large allocations get dedicated mmap'd regions.
// All memory is freed together with Reset().
//
// Zero heap allocations after NewPool.

package memory

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Pool manages an off-heap memory pool with mmap-backed slabs.
// Uses per-slab sharding for lock-free O(1) allocation in the hot path.
// CRITICAL: Allocations are 8-byte aligned for SIMD/ARM safety.
type Pool struct {
	cfg AllocatorConfig

	// Memory accounting (all atomic for lock-free reads)
	reserved  atomic.Uint64 // Total bytes mmap'd (physical limit)
	allocated atomic.Uint64 // Bytes allocated from slabs
	committed atomic.Uint64 // Bytes committed via mmap
	peak      atomic.Uint64 // Peak single allocation

	// Slab management: slabLen tracks the active count of slabs.
	// Readers slice slabBuf[:slabLen.Load()] — zero alloc.
	// slabBuf and slabStructs are pre-allocated once, never resized.
	slabLen     atomic.Int64
	slabBuf     []*slab // Pre-allocated backing array, capacity = maxSlabs
	slabStructs []slab  // Pre-allocated slab metadata, never reallocated
	// Hot slab cursor - atomic index for O(1) hot path lookup
	cursor atomic.Int64
	// Large allocations tracking: same zero-alloc pattern as slabs.
	largeLen     atomic.Int64
	largeBuf     []*slab
	largeStructs []slab
	largeMu      sync.Mutex // Serializes large allocation tracking
	// Serializes slab list expansion to prevent data race on shared slabBuf
	growMu sync.Mutex
	// Generation counter for Reset safety
	generation atomic.Uint64
	// Slab size and alignment
	align     uint64
	alignMask uint64
}

// slab represents an mmap-backed memory slab.
// DO NOT COPY: contains atomic.Uint64 which embeds sync.noCopy pragma.
type slab struct {
	data  []byte // Off-heap mmap'd data
	used  atomic.Uint64
	mmapd bool // Track if mmap'd (vs make([]byte))
}

// NewPool creates a new off-heap memory pool.
// Returns *Pool pointer - no global singleton race.
func NewPool(cfg AllocatorConfig) (*Pool, error) {
	if cfg.SlabCount <= 0 {
		cfg.SlabCount = 16
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 64 * 1024 * 1024
	}
	if cfg.SlabSize == 0 {
		cfg.SlabSize = 1024 * 1024 // 1MB slabs
	}

	// Validate huge page alignment when requested.
	// UseHugePages requires HugepageSize > 0; silently ignored on platforms
	// without huge page support (e.g. Darwin where HugepageSize == 0).
	if cfg.UseHugePages {
		if HugepageSize == 0 {
			// Huge pages not supported on this platform; silently disable
			cfg.UseHugePages = false
		} else if cfg.SlabSize%HugepageSize != 0 {
			return nil, fmt.Errorf("SlabSize must be a multiple of HugepageSize (%d bytes) when UseHugePages is enabled", HugepageSize)
		}
	}

	// Pre-allocate slabBuf backing array — single heap alloc, never resized.
	// maxSlabs = ceil(PoolSize / SlabSize), clamped to at least SlabCount.
	maxSlabs := int((cfg.PoolSize + cfg.SlabSize - 1) / cfg.SlabSize)
	if maxSlabs < cfg.SlabCount {
		maxSlabs = cfg.SlabCount
	}

	p := &Pool{
		cfg:         cfg,
		align:       8,
		alignMask:   7,
		slabBuf:       make([]*slab, maxSlabs),
		slabStructs:   make([]slab, maxSlabs),
		largeBuf:      make([]*slab, maxSlabs),
		largeStructs:  make([]slab, maxSlabs),
	}

	// Pre-allocate initial slabs if configured
	if cfg.Prealloc {
		totalPrealloc := uint64(cfg.SlabCount) * cfg.SlabSize
		if totalPrealloc > cfg.PoolSize {
			return nil, ErrPoolExhausted
		}

		for i := 0; i < cfg.SlabCount; i++ {
			data, err := p.mmapSlab(cfg.SlabSize)
			if err != nil {
				// Rollback: munmap already-allocated slabs
				for j := 0; j < i; j++ {
					if s := p.slabBuf[j]; s != nil && s.mmapd {
						unix.Munmap(s.data)
						p.reserved.Add(-cfg.SlabSize)
					}
				}
				return nil, ErrMmapFailed
			}
			s := &p.slabStructs[i]
			s.data = data
			s.mmapd = true
			s.used.Store(0)
			p.reserved.Add(cfg.SlabSize)
			p.slabBuf[i] = s
		}
		p.slabLen.Store(int64(cfg.SlabCount))
		p.cursor.Store(0)
	} else {
		p.slabLen.Store(0)
		p.cursor.Store(-1)
	}

	return p, nil
}

// mmapSlabBase is the base mmap implementation shared across platforms.
func (p *Pool) mmapSlabBase(slabSize uint64) ([]byte, error) {
	if slabSize > math.MaxInt {
		return nil, fmt.Errorf("slab size %d exceeds addressable int range", slabSize)
	}
	data, err := unix.Mmap(-1, 0, int(slabSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// reserve atomically reserves size bytes from the pool limit.
// Returns true if reservation succeeded, false if limit would be exceeded.
func (p *Pool) reserve(size uint64) bool {
	for {
		reserved := p.reserved.Load()
		// Check overflow: if size > PoolSize, or reserved > PoolSize - size,
		// the reservation would exceed the pool limit.
		if size > p.cfg.PoolSize || reserved > p.cfg.PoolSize-size {
			return false
		}
		if p.reserved.CompareAndSwap(reserved, reserved+size) {
			return true
		}
		// CAS failed: retry with updated reserved value
	}
}

// Allocate returns memory from the pool.
// Returns nil slice and ErrPoolExhausted if pool cannot expand.
// Hot path: O(1) via CAS on hot slab, no global locks.
func (p *Pool) Allocate(size uint64) ([]byte, error) {
	if size == 0 {
		return nil, ErrInvalidSize
	}

	// Large allocation - track separately for proper cleanup
	if size > p.cfg.SlabSize {
		return p.allocateLarge(size)
	}

	// Hot path: try hot slab first (no reservation needed, slabs already mmap'd)
	for {
		gen := p.generation.Load()
		slabs := p.slabBuf[:p.slabLen.Load()]

		cursor := p.cursor.Load()
		if cursor < 0 || cursor >= int64(len(slabs)) {
			break // Need to add first slab
		}

		s := slabs[cursor]
		if s == nil {
			break
		}

		used := s.used.Load()
		alignedUsed := (used + p.alignMask) &^ p.alignMask
		newUsed := alignedUsed + size

		// Overflow protection
		if newUsed < alignedUsed || newUsed > uint64(len(s.data)) {
			break // Hot slab full or overflow
		}

		// CAS to claim space in hot slab
		if s.used.CompareAndSwap(used, newUsed) {
			// Record allocation before gen check: memory is consumed regardless.
			// Conservative overcount is safer for monitoring than undercount.
			p.allocated.Add(size)

			// Post-CAS generation check: if Reset happened during CAS,
			// retry to avoid returning a pointer into memory being unmapped.
			if p.generation.Load() != gen {
				continue // Retry from slow path
			}
			return s.data[alignedUsed:newUsed], nil
		}
		// CAS failed: retry hot slab
	}

	// Slow path: scan for available space or add new slab
	return p.allocateSlowPath(size)
}

// allocateSlowPath handles allocation when hot slab is full.
// Uses atomic slice pointer swap to publish new slabs array without races.
func (p *Pool) allocateSlowPath(size uint64) ([]byte, error) {
retry:
	for {
		gen := p.generation.Load()
		slabs := p.slabBuf[:p.slabLen.Load()]

		// Scan all slabs for space
		for i, s := range slabs {
			if s == nil {
				continue
			}
			for {
				used := s.used.Load()
				alignedUsed := (used + p.alignMask) &^ p.alignMask
				newUsed := alignedUsed + size

				// Overflow protection
				if newUsed < alignedUsed || newUsed > uint64(len(s.data)) {
					break
				}

				// Pre-check is speculative only: Reset can still fire between
				// this load and the CAS. The post-CAS check below is the
				// load-bearing guarantee.

				if s.used.CompareAndSwap(used, newUsed) {
					// Record allocation before gen check: memory is consumed regardless.
					// Conservative overcount is safer for monitoring than undercount.
					p.allocated.Add(size)

					// Post-CAS generation check: if Reset happened during CAS,
					// retry to avoid returning a pointer into memory being unmapped.
					if p.generation.Load() != gen {
						continue retry
					}
					// Cursor only moves forward to avoid thrashing
					// under concurrent slab expansion
					for {
						oldCursor := p.cursor.Load()
						if int64(i) <= oldCursor {
							break
						}
						if p.cursor.CompareAndSwap(oldCursor, int64(i)) {
							break
						}
					}
					return s.data[alignedUsed:newUsed], nil
				}
			}
		}

		// No space — serialize slab list expansion to prevent
		// data race on shared slabBuf backing array.
		p.growMu.Lock()

		// Re-check after acquiring lock: another goroutine may have
		// already expanded the slab list while we were waiting.
		recheckSlabs := p.slabBuf[:p.slabLen.Load()]
		if len(recheckSlabs) > len(slabs) {
			p.growMu.Unlock()
			continue retry
		}

		slabSize := p.cfg.SlabSize
		if !p.reserve(slabSize) {
			p.growMu.Unlock()
			return nil, ErrPoolExhausted
		}

		data, err := p.mmapSlab(slabSize)
		if err != nil {
			p.reserved.Add(-slabSize) // Rollback reservation
			p.growMu.Unlock()
			return nil, ErrMmapFailed // Distinguish OS failure from pool limit
		}

		newIdx := len(recheckSlabs)

		// Check capacity before extending — if slabBuf is full, pool is exhausted.
		if newIdx >= cap(p.slabBuf) {
			unix.Munmap(data)
			p.reserved.Add(-slabSize)
			p.growMu.Unlock()
			return nil, ErrPoolExhausted
		}

		// Zero-alloc: reuse pre-allocated slab struct and slabBuf slot.
		s := &p.slabStructs[newIdx]
		s.data = data
		s.mmapd = true
		s.used.Store(size)
		p.slabBuf[newIdx] = s
		p.slabLen.Store(int64(newIdx + 1))
		p.growMu.Unlock()

		p.allocated.Add(size)

		// Update cursor to new slab using monotonic CAS
		for {
			oldCursor := p.cursor.Load()
			if int64(newIdx) <= oldCursor {
				break
			}
			if p.cursor.CompareAndSwap(oldCursor, int64(newIdx)) {
				break
			}
		}

		return data[:size], nil
	}
}

// allocateLarge handles allocations exceeding slab size via direct mmap.
// Tracks in large list for proper cleanup.
func (p *Pool) allocateLarge(size uint64) ([]byte, error) {
	// Reserve size from pool limit atomically
	if !p.reserve(size) {
		return nil, ErrPoolExhausted
	}

	data, err := p.mmapSlab(size)
	if err != nil {
		p.reserved.Add(-size)
		return nil, ErrMmapFailed
	}

	// Peak update only after mmap confirmed successful
	for {
		oldPeak := p.peak.Load()
		if size <= oldPeak {
			break
		}
		if p.peak.CompareAndSwap(oldPeak, size) {
			break
		}
	}

	p.committed.Add(size)
	p.allocated.Add(size)

	// Zero-alloc: reuse pre-allocated large slab struct.
	p.largeMu.Lock()
	idx := int(p.largeLen.Load())
	if idx >= len(p.largeStructs) {
		p.largeMu.Unlock()
		unix.Munmap(data)
		p.reserved.Add(-size)
		p.allocated.Add(-size)
		p.committed.Add(-size)
		return nil, ErrPoolExhausted
	}
	s := &p.largeStructs[idx]
	s.data = data
	s.mmapd = true
	p.largeBuf[idx] = s
	p.largeLen.Store(int64(idx + 1))
	p.largeMu.Unlock()

	return data, nil
}

// Reset releases all mmap'd memory and reinitializes the pool.
// WARNING: All outstanding allocations become invalid.
// Caller must ensure quiescence: no concurrent Allocate calls should be in flight.
// Generation counter catches stragglers still in their CAS retry loop.
// Note: Munmap errors are intentionally ignored — mappings are released
// on best-effort basis and will be reclaimed by the OS on process exit.
func (p *Pool) Reset() {
	// Increment generation - allocators will retry on old slabs
	p.generation.Add(1)

	// Unmap all slabs and nil out entries for GC
	slabs := p.slabBuf[:p.slabLen.Load()]
	for i := range slabs {
		if s := slabs[i]; s != nil && s.mmapd && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
		p.slabBuf[i] = nil
	}

	// Unmap large allocations
	largeLen := p.largeLen.Load()
	for i := int64(0); i < largeLen; i++ {
		if s := p.largeBuf[i]; s != nil && s.mmapd && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
		p.largeBuf[i] = nil
	}
	p.largeLen.Store(0)

	// Reset state
	p.reserved.Store(0)
	p.allocated.Store(0)
	p.committed.Store(0)
	p.peak.Store(0) // Clear peak tracking
	p.cursor.Store(-1)

	p.slabLen.Store(0)
}

// Stats returns current memory statistics.
// Safe for concurrent access - takes atomic snapshot.
func (p *Pool) Stats() PoolStats {
	slabLen := p.slabLen.Load()

	return PoolStats{
		Reserved:  p.reserved.Load(),
		Allocated: p.allocated.Load(),
		Committed: p.committed.Load(),
		PeakUsage: p.peak.Load(),
		SlabCount: int32(slabLen),
		SlabSize:  p.cfg.SlabSize,
		Align:     p.align,
	}
}

// PoolStats holds detailed memory pool statistics.
type PoolStats struct {
	Reserved  uint64 // Total bytes mmap'd (physical limit)
	Allocated  uint64 // Bytes actually allocated from slabs
	Committed  uint64 // Bytes mmap'd for large allocations
	PeakUsage  uint64 // Peak single large allocation
	SlabCount  int32
	SlabSize   uint64
	Align      uint64
}
