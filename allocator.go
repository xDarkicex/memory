// Package memory provides low-level memory management primitives for
// native systems environments. Implements off-heap allocation via mmap
// for complete GC isolation and deterministic runtime behavior.
package memory

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Error definitions - explicit errors for all failure modes.
var (
	ErrPoolExhausted = errors.New("pool exhausted: cannot expand under memory pressure")
	ErrInvalidSize   = errors.New("invalid allocation size: must be greater than 0")
	ErrArenaExhausted = errors.New("arena exhausted: insufficient space for allocation")
	ErrMmapFailed    = errors.New("mmap allocation failed: system limit or OOM")
)

// PageSize is the actual system page size obtained via OS syscall.
var PageSize = os.Getpagesize()

// HugepageSize is the standard huge page size on x86_64 (2MB).
// Used for validation when UseHugePages is enabled.
const HugepageSize uint64 = 2 * 1024 * 1024

// AllocatorConfig holds memory allocator settings.
type AllocatorConfig struct {
	// PoolSize is the maximum pool size in bytes (hard limit on mmap'd memory).
	PoolSize uint64
	// UseHugePages enables transparent huge pages (requires root/hugepage allocation).
	// When enabled, SlabSize should be a multiple of HugepageSize for best results.
	// On failure to allocate huge pages, falls back to regular mmap silently.
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

	// Slab management: atomic pointer to immutable slice of slab pointers
	slabs atomic.Pointer[[]*slab]
	// Hot slab cursor - atomic index for O(1) hot path lookup
	cursor atomic.Int64
	// Large allocations tracking (protected by dedicated mutex)
	largeMu sync.Mutex
	large   []*slab // Large allocations require explicit tracking for Reset/Free
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

	// Validate huge page alignment when requested
	if cfg.UseHugePages && cfg.SlabSize%HugepageSize != 0 {
		return nil, errors.New("SlabSize must be a multiple of HugepageSize (2MB) when UseHugePages is enabled")
	}

	p := &Pool{
		cfg:       cfg,
		align:     8,
		alignMask: 7,
	}

	// Pre-allocate initial slabs if configured
	if cfg.Prealloc {
		totalPrealloc := uint64(cfg.SlabCount) * cfg.SlabSize
		if totalPrealloc > cfg.PoolSize {
			return nil, ErrPoolExhausted
		}

		initialSlabs := make([]*slab, 0, cfg.SlabCount)
		for i := 0; i < cfg.SlabCount; i++ {
			data, err := p.mmapSlab(cfg.SlabSize)
			if err != nil {
				// On failure, clean up what we allocated and return error
				// Rollback reserved counter for each successful slab
				for j := 0; j < i; j++ {
					if s := initialSlabs[j]; s != nil && s.mmapd {
						unix.Munmap(s.data)
						p.reserved.Add(-cfg.SlabSize)
					}
				}
				return nil, ErrMmapFailed
			}
			newSlab := &slab{
				data:  data,
				mmapd: true,
			}
			newSlab.used.Store(0)
			p.reserved.Add(cfg.SlabSize)
			initialSlabs = append(initialSlabs, newSlab)
		}
		p.slabs.Store(&initialSlabs)
		p.cursor.Store(0) // First slab is hot
	} else {
		initialSlabs := make([]*slab, 0, cfg.SlabCount)
		p.slabs.Store(&initialSlabs)
		p.cursor.Store(-1)
	}

	return p, nil
}

// MAP_HUGETLB is used for huge page mappings on Linux.
// Requires root privileges or sufficient hugepage allocation.
// This is a runtime constant since unix.MAP_HUGETLB is not available on all platforms.
const MAP_HUGETLB = 0x40000

// mmapSlab creates a single mmap-backed slab.
// If UseHugePages is enabled, uses MAP_HUGETLB (requires root or sufficient privileges on Linux).
// When huge pages are requested but unavailable, falls back to regular mmap silently.
func (p *Pool) mmapSlab(slabSize uint64) ([]byte, error) {
	if p.cfg.UseHugePages {
		data, err := unix.Mmap(-1, 0, int(slabSize), unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_ANON|unix.MAP_PRIVATE|MAP_HUGETLB)
		if err != nil {
			// MAP_HUGETLB requires root or hugepage support; fall back to regular mmap
			return p.mmapSlabRegular(slabSize)
		}
		return data, nil
	}
	return p.mmapSlabRegular(slabSize)
}

// mmapSlabRegular creates a regular (non-hugepage) mmap-backed slab.
func (p *Pool) mmapSlabRegular(slabSize uint64) ([]byte, error) {
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
		if reserved+size > p.cfg.PoolSize {
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
		slabsPtr := p.slabs.Load()
		slabs := *slabsPtr

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

		// Check generation BEFORE mutating slab state (SC guarantees visibility)
		if p.generation.Load() != gen {
			continue // Reset in progress, retry from slow path
		}

		// CAS to claim space in hot slab
		if s.used.CompareAndSwap(used, newUsed) {
			// Record allocation first - memory IS allocated regardless of gen check.
			// Conservative: Stats().Allocated may overcount during Reset churn,
			// but never undercount (which would be dangerous for monitoring).
			p.allocated.Add(size)

			// Post-CAS generation check: if Reset happened during CAS,
			// retry without returning to avoid pointer into unmapped memory.
			if p.generation.Load() != gen {
				continue // Retry from slow path; allocated already counted
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
		slabsPtr := p.slabs.Load()
		slabs := *slabsPtr

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

				// Check generation BEFORE mutating slab state.
				// Go's atomic operations are sequentially consistent (SC),
				// guaranteeing the generation increment in Reset() is visible
				// before any subsequent load of slab pointers.
				if p.generation.Load() != gen {
					continue retry
				}

				if s.used.CompareAndSwap(used, newUsed) {
					// Record allocation first - memory IS allocated regardless of gen check.
					// This is conservative: Stats().Allocated may slightly overcount during
					// retries, but never undercounts (which would be dangerous for monitoring).
					p.allocated.Add(size)

					// Generation check after CAS for symmetry.
					// If Reset happened during CAS, retry without returning
					// to avoid returning slice into memory being unmapped.
					if p.generation.Load() != gen {
						// allocated.Add already recorded; on retry this slab's space
						// will appear occupied (which is fine - conservative accounting)
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

		// No space - reserve and add new slab
		slabSize := p.cfg.SlabSize
		if !p.reserve(slabSize) {
			return nil, ErrPoolExhausted
		}

		data, err := p.mmapSlab(slabSize)
		if err != nil {
			p.reserved.Add(-slabSize) // Rollback reservation
			return nil, ErrMmapFailed // Distinguish OS failure from pool limit
		}

		newSlab := &slab{
			data:  data,
			mmapd: true,
		}

		// Set used BEFORE publishing slab to avoid race window
		alignedUsed := uint64(0)
		newSlab.used.Store(alignedUsed + size)

		// Atomic snapshot swap: allocate new slice, copy, append
		newSlabs := make([]*slab, len(slabs)+1)
		copy(newSlabs, slabs)
		newSlabs[len(slabs)] = newSlab

		// Publish using heap pointer - slabsPtr is the address of the heap-allocated slice header
		if !p.slabs.CompareAndSwap(slabsPtr, &newSlabs) {
			// CAS failed: another goroutine added a slab, retry
			unix.Munmap(data)
			p.reserved.Add(-slabSize) // Rollback reservation
			continue retry
		}

		p.allocated.Add(size)

		// Update cursor to new slab using monotonic CAS
		newIdx := int64(len(slabs))
		for {
			oldCursor := p.cursor.Load()
			if newIdx <= oldCursor {
				break
			}
			if p.cursor.CompareAndSwap(oldCursor, newIdx) {
				break
			}
		}

		return data[alignedUsed : alignedUsed+size], nil
	}
}

// allocateLarge handles allocations exceeding slab size via direct mmap.
// Tracks in large list for proper cleanup.
func (p *Pool) allocateLarge(size uint64) ([]byte, error) {
	// Reserve size from pool limit atomically
	if !p.reserve(size) {
		return nil, ErrPoolExhausted
	}

	// Update peak tracking
	for {
		oldPeak := p.peak.Load()
		if size <= oldPeak {
			break
		}
		if p.peak.CompareAndSwap(oldPeak, size) {
			break
		}
	}

	data, err := p.mmapSlab(size)
	if err != nil {
		p.reserved.Add(-size) // Rollback reservation
		return nil, ErrMmapFailed
	}

	p.committed.Add(size)
	p.allocated.Add(size)

	// Track large allocation for Reset/Free
	newSlab := &slab{
		data:  data,
		mmapd: true,
		used:  atomic.Uint64{}, // Explicitly initialize for consistency
	}

	p.largeMu.Lock()
	p.large = append(p.large, newSlab)
	p.largeMu.Unlock()

	return data, nil
}

// Reset releases all mmap'd memory and reinitializes the pool.
// WARNING: All outstanding allocations become invalid.
// Caller must ensure quiescence: no concurrent Allocate calls should be in flight.
// Generation counter catches stragglers still in their CAS retry loop.
func (p *Pool) Reset() {
	// Increment generation - allocators will retry on old slabs
	p.generation.Add(1)

	// Unmap all slabs
	slabsPtr := p.slabs.Load()
	slabs := *slabsPtr
	for _, s := range slabs {
		if s != nil && s.mmapd && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
	}

	// Unmap large allocations
	p.largeMu.Lock()
	for _, s := range p.large {
		if s != nil && s.mmapd && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
	}
	p.large = nil
	p.largeMu.Unlock()

	// Reset state
	p.reserved.Store(0)
	p.allocated.Store(0)
	p.committed.Store(0)
	p.peak.Store(0) // Clear peak tracking
	p.cursor.Store(-1)

	// Reinitialize empty slabs array
	empty := make([]*slab, 0, p.cfg.SlabCount)
	p.slabs.Store(&empty)
}

// Stats returns current memory statistics.
// Safe for concurrent access - takes atomic snapshot.
func (p *Pool) Stats() PoolStats {
	slabsPtr := p.slabs.Load()
	slabs := *slabsPtr

	return PoolStats{
		Reserved:  p.reserved.Load(),
		Allocated: p.allocated.Load(),
		Committed: p.committed.Load(),
		PeakUsage: p.peak.Load(),
		SlabCount: int32(len(slabs)),
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

// Arena provides a thread-local memory arena with lock-free bump allocation.
// Uses off-heap mmap for complete GC isolation.
type Arena struct {
	offset atomic.Uint64
	data   []byte
	mmapd  bool
	align  uint64
}

// NewArena creates a new off-heap memory arena.
func NewArena(size uint64) (*Arena, error) {
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
			ptr := uintptr(unsafe.Pointer(&a.data[0])) + uintptr(newOffset)
			return unsafe.Pointer(ptr), nil
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

// MemoryHint provides hints to the memory system.
type MemoryHint int

const (
	HintNormal MemoryHint = iota
	HintWillNeed
	HintDontNeed
)

// Hint passes hints to the OS memory manager via direct unix.Madvise syscall.
func Hint(h MemoryHint, ptr unsafe.Pointer, length int) {
	var advice int
	switch h {
	case HintWillNeed:
		advice = unix.MADV_WILLNEED
	case HintDontNeed:
		advice = unix.MADV_DONTNEED
	default:
		advice = unix.MADV_NORMAL
	}
	pageSize := uintptr(PageSize)
	pageBase := (uintptr(ptr) / pageSize) * pageSize
	// Account for ptr offset within page when calculating length
	offset := uintptr(ptr) - pageBase
	pageLen := (offset + uintptr(length) + pageSize - 1) &^ (pageSize - 1)

	_ = unix.Madvise(unsafe.Slice((*byte)(unsafe.Pointer(pageBase)), pageLen), advice)
}

// GCStats holds garbage collector statistics.
type GCStats struct {
	PauseTotal time.Duration
	PauseLast  time.Duration
	NumGC      uint32
	Forced     bool
}

// ReadGCStats reads current GC statistics using NumForcedGC.
func ReadGCStats() GCStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return GCStats{
		PauseTotal: time.Duration(m.PauseTotalNs),
		PauseLast:  time.Duration(m.PauseNs[m.NumGC%256]),
		NumGC:      m.NumGC,
		Forced:     m.NumForcedGC > 0,
	}
}

// Profile records memory profile data.
type Profile struct {
	Alloc      uint64
	TotalAlloc uint64
	Sys        uint64
	Lookups    uint64
	Mallocs    uint64
	Frees      uint64
}

// ReadProfile reads current memory profile.
func ReadProfile() Profile {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Profile{
		Alloc:      m.Alloc,
		TotalAlloc: m.TotalAlloc,
		Sys:        m.Sys,
		Lookups:    m.Lookups,
		Mallocs:    m.Mallocs,
		Frees:      m.Frees,
	}
}

// ZeroMemory securely zeros a memory region.
func ZeroMemory(p unsafe.Pointer, n uintptr) {
	if n > 0 {
		clear(unsafe.Slice((*byte)(p), n))
	}
}

// MemStats provides system memory statistics.
type MemStats struct {
	Total     uint64
	Available uint64
	Used      uint64
	Free      uint64
	SwapTotal uint64
	SwapUsed  uint64
	Cached    uint64
	Buffers   uint64
}

// ReadMemStats reads system memory statistics.
// Returns Go heap metrics - for true system memory use unix.Sysinfo on Linux.
func ReadMemStats() MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return MemStats{
		Total:     m.HeapSys,
		Available: m.HeapIdle,
		Used:      m.HeapInuse,
		Free:      m.HeapIdle,
		SwapTotal: 0,
		SwapUsed:  0,
		Cached:    m.HeapReleased,
		Buffers:   0,
	}
}

// Watchdog monitors memory pressure and triggers callbacks.
// Singleton with CAS-based replacement.
var globalWatchdog atomic.Pointer[Watchdog]

// Watchdog monitors system memory pressure.
type Watchdog struct {
	threshold uint64
	action    func(MemStats)
	stop      chan struct{}
	stopOnce  sync.Once
}

// NewWatchdog creates a new memory watchdog.
func NewWatchdog(threshold uint64, action func(MemStats)) *Watchdog {
	return &Watchdog{
		threshold: threshold,
		action:    action,
		stop:      make(chan struct{}),
	}
}

// Start begins memory monitoring.
func (w *Watchdog) Start() {
	go w.run()
}

// Stop stops monitoring safely - idempotent via sync.Once.
func (w *Watchdog) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

func (w *Watchdog) run() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			stats := ReadMemStats()
			if stats.Used > w.threshold {
				w.action(stats)
			}
		}
	}
}

// RegisterMemoryPressureCallback sets the threshold callback.
// Uses actual CAS loop for atomic watchdog replacement.
// Returns a stop function to cleanly shut down the watchdog.
func RegisterMemoryPressureCallback(threshold uint64, fn func(MemStats)) func() {
	wd := NewWatchdog(threshold, fn)

	// CAS loop for atomic replacement
	for {
		old := globalWatchdog.Load()

		// Try to atomically replace old with new
		if globalWatchdog.CompareAndSwap(old, wd) {
			if old != nil {
				old.Stop()
			}
			break
		}
		// CAS failed: another goroutine replaced it, retry
	}

	wd.Start()
	return wd.Stop
}
