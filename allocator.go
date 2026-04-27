// Package memory provides low-level memory management primitives for
// native systems environments. Implements off-heap allocation via mmap
// for complete GC isolation and deterministic runtime behavior.
package memory

import (
	"errors"
	"fmt"
	"math"
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

// Hint is defined in memory_linux.go and memory_darwin.go based on platform.

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

// ReadMemStats reads Go heap memory statistics.
// Note: this reports Go runtime heap metrics, not physical RAM.
// For off-heap mmap'd memory managed by this allocator, look at PoolStats.
func ReadMemStats() MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return MemStats{
		Total:     m.HeapSys,     // Total memory obtained from OS
		Available: m.HeapSys,    // Total available (same as Total for heap)
		Used:      m.HeapInuse,  // In-use by runtime allocator
		Free:      m.HeapIdle,   // Memory not used by runtime
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
