// Package memory — freelist allocator.
//
// FreeList is a fixed-size, lock-free, off-heap allocator backed by mmap.
// Every allocation returns a slot of exactly SlotSize bytes. Deallocate
// returns the slot to the pool for reuse. The Go GC never scans this memory.
//
// Use when:
//   - Homogeneous objects with independent lifetimes (network buffers,
//     DB page caches, object pools too large for sync.Pool)
//   - Per-object free is required (Arena/Pool only support bulk Reset)
//   - GC isolation matters
//
// Do NOT use when:
//   - Sizes vary — use Pool
//   - All lifetimes are scoped together — Arena or Pool.Reset() is simpler
//   - Allocations are tiny and short-lived — Go's stack allocator wins
//
// Sharp edges:
//   - Double-free silently corrupts the freelist. Best-effort detection via
//     per-slot generation counter; not a 100% guarantee.
//   - Use-after-free is undefined behavior (segfault or silent corruption).
//   - ABA problem on the freelist head is mitigated by a tagged pointer
//     packing a 16-bit generation counter into the upper bits of the
//     uint64 CAS word. Safe on 48-bit virtual address systems (ARM64, x86_64).

// Safety status: SCAFFOLD — needs hardening.
//   - Tagged-pointer ABA protection is implemented but not yet fuzzed under
//     high-contention concurrent deallocation loops.
//   - Double-free detection is a hardening TODO; currently trusts caller.
//   - 48-bit VA assumption validated at init on darwin; Linux accepts
//     the documented risk (LA57 systems with 57-bit VA will corrupt tags).
//   - Slab tracking for Free uses a mutex; Reset is not concurrent-safe
//     (same contract as Pool.Reset).

package memory

import (
	"errors"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	ErrFreelistExhausted   = errors.New("freelist exhausted: pool limit reached")
	ErrDoubleDeallocation  = errors.New("double deallocation detected")
	ErrInvalidDeallocation = errors.New("invalid deallocation: pointer not owned by this freelist")
)

// FreeListConfig holds configuration for a fixed-size freelist allocator.
type FreeListConfig struct {
	// PoolSize is the hard limit on total mmap'd bytes.
	PoolSize uint64
	// SlotSize is the fixed size of each allocation slot.
	// Must be >= 8 (minimum for intrusive freelist pointer).
	SlotSize uint64
	// SlabSize is the size of each mmap'd slab region.
	// Should be a multiple of SlotSize for zero waste; defaults to 1MB.
	SlabSize uint64
	// SlabCount is the initial number of slab descriptors to pre-allocate.
	SlabCount int
	// Prealloc eagerly allocates SlabCount slabs at creation time.
	Prealloc bool
}

// DefaultFreeListConfig returns a sensible default configuration.
func DefaultFreeListConfig() FreeListConfig {
	return FreeListConfig{
		PoolSize:  64 * 1024 * 1024,
		SlotSize:  4096,
		SlabSize:  1024 * 1024,
		SlabCount: 16,
		Prealloc:  false,
	}
}

// FreeList is a lock-free, fixed-size, off-heap allocator.
//
// Slots are threaded into an intrusive singly-linked free list. The head
// pointer is a tagged uint64 encoding (generation << 48) | pointer to
// provide ABA protection on CAS. Allocate pops the head; Deallocate pushes
// back. When the free list is empty, a new slab is mmap'd.
type FreeList struct {
	cfg FreeListConfig

	// Freelist head: tagged pointer for ABA-safe CAS.
	head atomic.Uint64

	// Accounting (all atomic for lock-free reads).
	reserved  atomic.Uint64
	allocated atomic.Uint64 // Active (allocated, not yet freed) bytes

	// Generation counter for Free/Reset safety (not the same as ABA tag).
	// Incremented on Free/Reset to invalidate in-flight allocations.
	generation atomic.Uint64

	// Slab tracking: pre-allocated backing array, atomic length.
	// Matches Pool.slabBuf pattern — zero heap allocs after NewFreeList.
	slabMu  sync.Mutex
	slabBuf []*freelistSlab
	slabLen atomic.Int32
	slabCap int

	// Pre-computed values.
	slotsPerSlab uint64
	align        uint64
}

// freelistSlab represents a single mmap'd region divided into fixed-size slots.
type freelistSlab struct {
	data  []byte
	slots int
}

// NewFreeList creates a new fixed-size freelist allocator.
func NewFreeList(cfg FreeListConfig) (*FreeList, error) {
	if cfg.SlotSize < 8 {
		cfg.SlotSize = 8
	}
	if cfg.SlabSize == 0 {
		cfg.SlabSize = 1024 * 1024
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 64 * 1024 * 1024
	}
	if cfg.SlabCount <= 0 {
		cfg.SlabCount = 16
	}

	// Align slot size up to 8 bytes for pointer atomicity.
	align := uint64(8)
	slotSize := (cfg.SlotSize + align - 1) &^ (align - 1)

	slotsPerSlab := cfg.SlabSize / slotSize
	if slotsPerSlab == 0 {
		return nil, errors.New("SlabSize must be >= SlotSize")
	}

	// Pre-allocate slab descriptor array — single heap alloc, never resized.
	maxSlabs := int((cfg.PoolSize + cfg.SlabSize - 1) / cfg.SlabSize)
	if maxSlabs < cfg.SlabCount {
		maxSlabs = cfg.SlabCount
	}

	fl := &FreeList{
		cfg:          cfg,
		slotsPerSlab: slotsPerSlab,
		align:        align,
		slabBuf:      make([]*freelistSlab, maxSlabs),
		slabCap:      maxSlabs,
	}
	fl.cfg.SlotSize = slotSize

	if cfg.Prealloc {
		for i := 0; i < cfg.SlabCount; i++ {
			if err := fl.growSlab(); err != nil {
				fl.Reset()
				return nil, err
			}
		}
	}

	return fl, nil
}

// reserve atomically reserves size bytes from the pool limit.
func (fl *FreeList) reserve(size uint64) bool {
	for {
		reserved := fl.reserved.Load()
		if size > fl.cfg.PoolSize || reserved > fl.cfg.PoolSize-size {
			return false
		}
		if fl.reserved.CompareAndSwap(reserved, reserved+size) {
			return true
		}
	}
}

// growSlab mmap's a new slab and publishes all its slots onto the free list.
//
// Double-check locking: after acquiring slabMu, verifies the freelist is
// still empty — another goroutine may have populated it while we waited.
// Slots are published while holding slabMu to prevent Reset() from
// interleaving (which would SIGSEGV on munmap'd memory).
func (fl *FreeList) growSlab() error {
	slabSize := fl.cfg.SlabSize
	if !fl.reserve(slabSize) {
		return ErrFreelistExhausted
	}

	data, err := unix.Mmap(-1, 0, int(slabSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		fl.reserved.Add(-slabSize)
		return ErrMmapFailed
	}

	slotSize := fl.cfg.SlotSize
	slots := int(fl.slotsPerSlab)

	fl.slabMu.Lock()

	// Double-check: another goroutine may have populated the freelist
	// while we waited for the mutex (thundering herd guard).
	if unpackPtr(fl.head.Load()) != nil {
		fl.slabMu.Unlock()
		unix.Munmap(data)
		fl.reserved.Add(-slabSize)
		return nil // freelist already populated, caller will retry popFree
	}

	// Zero-alloc extend: reuse pre-allocated slabBuf slot.
	idx := int(fl.slabLen.Load())
	if idx >= fl.slabCap {
		fl.slabMu.Unlock()
		unix.Munmap(data)
		fl.reserved.Add(-slabSize)
		return ErrFreelistExhausted
	}

	slab := &freelistSlab{data: data, slots: slots}
	fl.slabBuf[idx] = slab
	fl.slabLen.Store(int32(idx + 1))

	// Publish all slots onto the free list while still holding slabMu.
	// This prevents Reset() from munmap'ing the slab mid-publish (SIGSEGV).
	// Reverse order so the first allocation gets the lowest-address slot.
	for i := slots - 1; i >= 0; i-- {
		ptr := unsafe.Add(unsafe.Pointer(&data[0]), uintptr(i)*uintptr(slotSize))
		fl.pushFree(ptr)
	}

	fl.slabMu.Unlock()
	return nil
}

// === Tagged pointer operations ===

const (
	tagShift = 48
	ptrMask  = (1 << 48) - 1
)

// packTaggedPtr packs a pointer and 16-bit generation into a uint64.
// Assumes <=48-bit virtual addresses (valid on ARM64 and x86_64 without LA57).
func packTaggedPtr(ptr unsafe.Pointer, gen uint16) uint64 {
	p := uintptr(ptr)
	return (uint64(p) & ptrMask) | (uint64(gen) << tagShift)
}

// unpackPtr extracts the pointer from a tagged uint64.
func unpackPtr(tagged uint64) unsafe.Pointer {
	return unsafe.Pointer(uintptr(tagged & ptrMask))
}

// unpackTag extracts the generation from a tagged uint64.
func unpackTag(tagged uint64) uint16 {
	return uint16(tagged >> tagShift)
}

// pushFree pushes a slot onto the free list.
// Uses atomic.StorePointer for the intrusive next pointer to avoid
// data races with concurrent popFree readers.
func (fl *FreeList) pushFree(ptr unsafe.Pointer) {
	for {
		old := fl.head.Load()
		oldTag := unpackTag(old)
		newTag := oldTag + 1

		// Atomic store: publish old head into the freed slot.
		// Concurrent popFree uses atomic.LoadPointer on the same word.
		atomic.StorePointer((*unsafe.Pointer)(ptr), unpackPtr(old))

		newTagged := packTaggedPtr(ptr, newTag)
		if fl.head.CompareAndSwap(old, newTagged) {
			return
		}
	}
}

// popFree pops a slot from the free list.
// Returns nil if the list is empty.
// Uses atomic.LoadPointer for the intrusive next pointer to avoid
// data races with concurrent pushFree writers.
func (fl *FreeList) popFree() unsafe.Pointer {
	for {
		old := fl.head.Load()
		ptr := unpackPtr(old)
		if ptr == nil {
			return nil
		}
		oldTag := unpackTag(old)
		newTag := oldTag + 1

		// Atomic load: read next pointer from the slot at head.
		// Concurrent pushFree uses atomic.StorePointer on the same word.
		next := atomic.LoadPointer((*unsafe.Pointer)(ptr))

		newTagged := packTaggedPtr(next, newTag)
		if fl.head.CompareAndSwap(old, newTagged) {
			return ptr
		}
	}
}

// === Public API ===

// Allocate returns a fixed-size off-heap memory slot.
// Returns nil and ErrFreelistExhausted if the pool limit is reached.
func (fl *FreeList) Allocate() ([]byte, error) {
	gen := fl.generation.Load()

	for {
		ptr := fl.popFree()
		if ptr == nil {
			if err := fl.growSlab(); err != nil {
				return nil, err
			}
			continue
		}

		// Post-pop generation check: if Reset/Free incremented generation
		// during popFree, push the slot back and retry with a fresh gen.
		if fl.generation.Load() != gen {
			fl.pushFree(ptr)
			gen = fl.generation.Load()
			continue
		}

		slotSize := fl.cfg.SlotSize
		fl.allocated.Add(slotSize)

		return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
	}
}

// Deallocate returns a slot to the free list.
// The caller must NOT access the slot after deallocation.
//
// Validates that the pointer belongs to a slab managed by this FreeList.
// Returns ErrInvalidDeallocation for external pointers or nil/empty slices.
//
// TODO(hardening): add per-slot generation counter for double-free detection.
func (fl *FreeList) Deallocate(slot []byte) error {
	if len(slot) == 0 {
		return ErrInvalidDeallocation
	}

	ptr := unsafe.Pointer(unsafe.SliceData(slot))

	if !fl.owns(ptr) {
		return ErrInvalidDeallocation
	}

	fl.allocated.Add(-fl.cfg.SlotSize)
	fl.pushFree(ptr)
	return nil
}

// owns returns true if ptr falls within a tracked slab and is aligned
// to the slot boundary.
func (fl *FreeList) owns(ptr unsafe.Pointer) bool {
	p := uintptr(ptr)
	n := int(fl.slabLen.Load())
	for i := 0; i < n; i++ {
		s := fl.slabBuf[i]
		if s == nil {
			continue
		}
		base := uintptr(unsafe.Pointer(&s.data[0]))
		end := base + uintptr(len(s.data))
		if p >= base && p < end {
			offset := p - base
			return offset%uintptr(fl.cfg.SlotSize) == 0
		}
	}
	return false
}

// Stats returns a point-in-time snapshot of allocator state.
// Safe for concurrent access — all fields are atomic reads.
func (fl *FreeList) Stats() FreeListStats {
	return FreeListStats{
		Reserved:  fl.reserved.Load(),
		Allocated: fl.allocated.Load(),
		SlotSize:  fl.cfg.SlotSize,
		SlabCount: fl.slabLen.Load(),
	}
}

// FreeListStats holds allocator statistics.
type FreeListStats struct {
	Reserved  uint64
	Allocated uint64
	SlotSize  uint64
	SlabCount int32
}

// Reset releases all slabs and reinitializes the free list to empty.
//
// WARNING: All outstanding allocations become invalid. The caller must
// ensure quiescence — no concurrent Allocate or Deallocate calls.
func (fl *FreeList) Reset() {
	fl.generation.Add(1)

	fl.slabMu.Lock()
	n := int(fl.slabLen.Load())
	for i := 0; i < n; i++ {
		if s := fl.slabBuf[i]; s != nil && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
		fl.slabBuf[i] = nil
	}
	fl.slabLen.Store(0)
	fl.slabMu.Unlock()

	fl.head.Store(0)
	fl.reserved.Store(0)
	fl.allocated.Store(0)
}

// Free releases all mmap'd memory. The FreeList must not be used after Free.
func (fl *FreeList) Free() error {
	fl.generation.Add(1)

	fl.slabMu.Lock()
	n := int(fl.slabLen.Load())
	for i := 0; i < n; i++ {
		if s := fl.slabBuf[i]; s != nil && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
	}
	fl.slabLen.Store(0)
	fl.slabMu.Unlock()

	fl.head.Store(0)
	fl.reserved.Store(0)
	fl.allocated.Store(0)
	return nil
}

// PreallocSlabCount reports the number of allocated slabs.
func (fl *FreeList) PreallocSlabCount() int {
	return int(fl.slabLen.Load())
}

// SlotSize returns the aligned slot size.
func (fl *FreeList) SlotSize() uint64 {
	return fl.cfg.SlotSize
}

// SlabSize returns the configured slab size.
func (fl *FreeList) SlabSize() uint64 {
	return fl.cfg.SlabSize
}
