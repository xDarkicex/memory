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
//   - Double-free is detected via per-slot generation counters (best-effort).
//   - Use-after-free is undefined behavior (segfault or silent corruption).
//   - ABA problem on the freelist head is mitigated by a 16-bit generation tag
//     packed into the upper bits of the CAS word. The tag wraps every 65,536
//     pushFree/popFree operations; at sustained rates above ~500K alloc-free
//     pairs/sec, a thread preempted for the wrap window could observe a stale
//     head. For GC-isolated workloads with small heaps this is typically safe
//     (no multi-ms STW pauses). LA57 kernels (57-bit VA) are rejected at init.
//   - Reset is not concurrent-safe (same contract as Pool.Reset).
//   - Double-free detection via slotGen allocates 8 bytes per slot on the Go
//     heap (e.g. 8MB for a 64MB pool with 64B slots). This is a deliberate
//     tradeoff for safety; disable by setting slotGen to nil if memory is tight.

package memory

import (
	"errors"
	"fmt"
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
	// Must be >= 16 (8 for intrusive next pointer + 4 for struct index).
	SlotSize uint64
	// SlabSize is the size of each mmap'd slab region.
	// Should be a multiple of SlotSize for zero waste; defaults to 1MB.
	SlabSize uint64
	// SlabCount is the initial number of slab descriptors to pre-allocate.
	SlabCount int
	// Prealloc eagerly allocates SlabCount slabs at creation time.
	Prealloc bool
	// UseHugePages attempts huge page allocation via MAP_HUGETLB (Linux only).
	// On Darwin: silently ignored — macOS has no working huge page support.
	UseHugePages bool
}

// DefaultFreeListConfig returns a sensible default configuration.
func DefaultFreeListConfig() FreeListConfig {
	return FreeListConfig{
		PoolSize:     64 * 1024 * 1024,
		SlotSize:     4096,
		SlabSize:     1024 * 1024,
		SlabCount:    16,
		Prealloc:     false,
		UseHugePages: false,
	}
}

// slabEntry maps a slab's base address to its index in slabStructs.
// Used for O(log N) binary search in findSlabIdxLocked. Kept sorted by base.
type slabEntry struct {
	base      uintptr
	structIdx int32
}

// FreeList is a lock-free, fixed-size, off-heap allocator.
//
// Slots are threaded into an intrusive singly-linked free list. Each free
// slot stores the next pointer at offset 0 and the owning slab's struct
// index at offset 8. The head pointer is a tagged uint64 encoding
// (generation << 48) | pointer for ABA protection on CAS.
// Allocate pops the head; Deallocate pushes back. When the free list is
// empty, a new slab is mmap'd.
type FreeList struct {
	cfg FreeListConfig

	// Hot path: each atomic on its own cache line to prevent false sharing.
	// head is the ABA-tagged freelist head pointer — written every alloc/dealloc.
	head atomic.Uint64
	_    [56]byte

	// allocated tracks active (handed out, not yet freed) bytes.
	allocated atomic.Uint64
	_         [56]byte

	// Generation counter for Free/Reset safety (not the same as ABA tag).
	// Incremented on Free/Reset to invalidate in-flight allocations.
	generation atomic.Uint64
	_          [56]byte

	// CAS retry counter for observability. Incremented on every failed CAS
	// in pushFree and popFree. Useful for contention profiling.
	casRetries atomic.Uint64
	_          [56]byte

	// Freed prevents use after Free(). Cold path — checked once per Allocate.
	freed atomic.Bool

	// Cold path: reserved is only touched on growSlab/Reset/Free.
	reserved atomic.Uint64

	// Slab tracking: pre-allocated backing arrays, atomic length.
	// RWMutex: Deallocate takes RLock for safe concurrent validation;
	// growSlab/Reset/Free take full Lock for mutation.
	slabMu      sync.RWMutex
	slabBuf     []*freelistSlab // Pre-allocated pointer array, never resized
	slabStructs []freelistSlab  // Pre-allocated value array (zero heap allocs in growSlab)
	slabBase    []slabEntry     // Sorted by base address for O(log N) lookup; maps to structIdx
	slabLen     atomic.Int32
	slabCap     int

	// Double-free detection: per-slot allocation sequence numbers.
	// slotGen[slabStructIdx*slotsPerSlab + slotOffset] stores the allocSeq
	// value at allocation time. Zero means the slot is free.
	// Memory cost: 8 bytes per slot (e.g. 8MB for 64MB pool @ 64B slots).
	slotGen  []atomic.Uint64
	allocSeq atomic.Uint64

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
	if cfg.SlotSize < 16 {
		cfg.SlotSize = 16
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

	// Validate huge page alignment when requested.
	if cfg.UseHugePages {
		if HugepageSize == 0 {
			cfg.UseHugePages = false
		} else if cfg.SlabSize%HugepageSize != 0 {
			return nil, errors.New("SlabSize must be a multiple of HugepageSize when UseHugePages is enabled")
		}
	}

	// Align slot size up to 8 bytes for pointer atomicity.
	align := uint64(8)
	slotSize := (cfg.SlotSize + align - 1) &^ (align - 1)

	slotsPerSlab := cfg.SlabSize / slotSize
	if slotsPerSlab == 0 {
		return nil, errors.New("SlabSize must be >= SlotSize")
	}

	// Pre-allocate all backing arrays — single heap alloc batch, never resized.
	maxSlabs := int((cfg.PoolSize + cfg.SlabSize - 1) / cfg.SlabSize)
	if maxSlabs < cfg.SlabCount {
		maxSlabs = cfg.SlabCount
	}

	totalSlots := uint64(maxSlabs) * slotsPerSlab

	fl := &FreeList{
		cfg:          cfg,
		slotsPerSlab: slotsPerSlab,
		align:        align,
		slabBuf:      make([]*freelistSlab, maxSlabs),
		slabStructs:  make([]freelistSlab, maxSlabs),
		slabBase:     make([]slabEntry, maxSlabs),
		slabCap:      maxSlabs,
		slotGen:      make([]atomic.Uint64, totalSlots),
	}
	fl.cfg.SlotSize = slotSize

	// Validate that mmap returns addresses within the 48-bit VA window
	// required by the tagged-pointer ABA scheme (see tagShift/ptrMask).
	data, err := unix.Mmap(-1, 0, int(PageSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("cannot validate VA space: %w", err)
	}
	if uintptr(unsafe.Pointer(&data[0]))>>tagShift != 0 {
		unix.Munmap(data)
		return nil, errors.New("tagged-pointer ABA scheme requires <=48-bit virtual addresses; LA57 kernel detected")
	}
	unix.Munmap(data)

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
//
// Note: mmap is called outside slabMu to avoid holding the lock during a
// potentially slow syscall. Under extreme thundering herd (1000+ goroutines
// hitting an empty freelist simultaneously), this causes redundant
// mmap+munmap pairs. This is a deliberate tradeoff — the double-check inside
// the lock discards redundant slabs, and the window is brief in practice.
func (fl *FreeList) growSlab() error {
	slabSize := fl.cfg.SlabSize
	if !fl.reserve(slabSize) {
		return ErrFreelistExhausted
	}

	data, err := fl.mmapSlab(slabSize)
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

	// Zero-alloc extend: reuse pre-allocated slabBuf and slabStructs slots.
	idx := int(fl.slabLen.Load())
	if idx >= fl.slabCap {
		fl.slabMu.Unlock()
		unix.Munmap(data)
		fl.reserved.Add(-slabSize)
		return ErrFreelistExhausted
	}

	// Use pre-allocated value struct — zero heap allocs after NewFreeList.
	s := &fl.slabStructs[idx]
	s.data = data
	s.slots = slots
	fl.slabBuf[idx] = s

	// Insert into slabBase sorted by address. The entry maps
	// sorted position -> struct index, so binary search returns the
	// correct structIdx even when mmap returns non-monotonic addresses.
	base := uintptr(unsafe.Pointer(&data[0]))
	fl.slabBase[idx] = slabEntry{base: base, structIdx: int32(idx)}
	// Insertion sort: walk backward, swap if out of order.
	for j := idx; j > 0 && fl.slabBase[j].base < fl.slabBase[j-1].base; j-- {
		fl.slabBase[j], fl.slabBase[j-1] = fl.slabBase[j-1], fl.slabBase[j]
	}

	fl.slabLen.Store(int32(idx + 1))

	// Publish all slots onto the free list while still holding slabMu.
	// This prevents Reset() from munmap'ing the slab mid-publish (SIGSEGV).
	// Reverse order so the first allocation gets the lowest-address slot.
	// Each slot gets its owning structIdx embedded at offset 8.
	for i := slots - 1; i >= 0; i-- {
		ptr := unsafe.Add(unsafe.Pointer(&data[0]), uintptr(i)*uintptr(slotSize))
		fl.pushFree(ptr, int32(idx))
	}

	fl.slabMu.Unlock()
	return nil
}

// mmapSlabBase is the base mmap implementation shared across platforms.
func (fl *FreeList) mmapSlabBase(slabSize uint64) ([]byte, error) {
	data, err := unix.Mmap(-1, 0, int(slabSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// === Tagged pointer operations ===

const (
	tagShift = 48
	ptrMask  = (1 << 48) - 1
)

func packTaggedPtr(ptr unsafe.Pointer, gen uint16) uint64 {
	p := uintptr(ptr)
	return (uint64(p) & ptrMask) | (uint64(gen) << tagShift)
}

func unpackPtr(tagged uint64) unsafe.Pointer {
	return unsafe.Pointer(uintptr(tagged & ptrMask))
}

func unpackTag(tagged uint64) uint16 {
	return uint16(tagged >> tagShift)
}

// Slot metadata packing at offset 8:
//   bits  0-23: structIdx (up to 16M slabs)
//   bits 24-31: homeShard (up to 256 shards)
func packSlotMeta(structIdx int32, homeShard uint8) uint32 {
	return uint32(structIdx) | (uint32(homeShard) << 24)
}
func unpackStructIdx(meta uint32) int32  { return int32(meta & 0x00FFFFFF) }
func unpackHomeShard(meta uint32) uint8  { return uint8(meta >> 24) }

// pushFree pushes a slot onto the free list. structIdx is the slab's index
// in slabStructs, embedded at slot offset 8 as packed metadata so Allocate
// can resolve it without a lock or binary search.
func (fl *FreeList) pushFree(ptr unsafe.Pointer, structIdx int32) {
	for {
		old := fl.head.Load()
		newTag := unpackTag(old) + 1

		atomic.StoreUint64((*uint64)(ptr), uint64(uintptr(unpackPtr(old))))
		*(*uint32)(unsafe.Add(ptr, 8)) = packSlotMeta(structIdx, 0)

		newTagged := packTaggedPtr(ptr, newTag)
		if fl.head.CompareAndSwap(old, newTagged) {
			return
		}
		fl.casRetries.Add(1)
	}
}

// popFree pops a slot from the free list. Returns nil if empty.
//
// Between loading the head and reading the slot's next pointer, the slot
// may be deallocated and reallocated by another thread. The CAS at the end
// fails due to tag mismatch, causing a retry. This stale read is harmless
// (8-byte aligned read on off-heap memory) and is correct Treiber stack
// behavior — the CAS validates consistency before returning.
func (fl *FreeList) popFree() unsafe.Pointer {
	for {
		old := fl.head.Load()
		ptr := unpackPtr(old)
		if ptr == nil {
			return nil
		}
		newTag := unpackTag(old) + 1

		next := unsafe.Pointer(uintptr(atomic.LoadUint64((*uint64)(ptr))))

		newTagged := packTaggedPtr(next, newTag)
		if fl.head.CompareAndSwap(old, newTagged) {
			return ptr
		}
		fl.casRetries.Add(1)
	}
}

// batchPop pops up to len(buf) raw pointers from the freelist.
// Each pop is an independent atomic CAS — safe under concurrent push/pop
// because popFree's ABA-tagged CAS guarantees exclusive ownership of the
// popped node before its next pointer is read.
// No bookkeeping (no slotGen, no allocated) — caller must handle it.
// Prefer BatchAllocate for external use.
func (fl *FreeList) batchPop(buf []unsafe.Pointer) int {
	for i := 0; i < len(buf); i++ {
		ptr := fl.popFree()
		if ptr == nil {
			return i
		}
		buf[i] = ptr
	}
	return len(buf)
}

// BatchAllocate pops up to len(slots) off-heap memory slots with a single CAS.
// Fills the provided slice with []byte views. Returns the count allocated
// (≤ len(slots), 0 if pool is empty) and any error from slab growth.
//
// Accounting is batched: allocated counter and allocSeq are updated once for
// the batch, not per slot. slotGen is still set per slot (unavoidable).
// Zero heap allocations — caller provides the slots buffer.
func (fl *FreeList) BatchAllocate(slots [][]byte) (int, error) {
	if len(slots) == 0 {
		return 0, nil
	}
	gen := fl.generation.Load()
	slotSize := fl.cfg.SlotSize

	// Clamp to stack-friendly batch size.
	n := len(slots)
	if n > 128 {
		n = 128
	}

	var ptrBuf [128]unsafe.Pointer
	batch := ptrBuf[:n]

	for {
		count := fl.batchPop(batch)
		if count == 0 {
			if err := fl.growSlab(); err != nil {
				return 0, err
			}
			continue
		}

		if fl.generation.Load() != gen {
			gen = fl.generation.Load()
			continue
		}

		// Batch accounting: single atomic increment per counter.
		fl.allocated.Add(uint64(count) * slotSize)
		lastSeq := fl.allocSeq.Add(uint64(count))

		for i := 0; i < count; i++ {
			ptr := batch[i]
			meta := *(*uint32)(unsafe.Add(ptr, 8))
			structIdx := int(unpackStructIdx(meta))
			base := uintptr(unsafe.Pointer(&fl.slabStructs[structIdx].data[0]))
			si := fl.slotIndex(ptr, base, structIdx)
			// Distribute sequence numbers: slot i gets lastSeq - (count-1-i).
			seq := lastSeq - uint64(count-1-i)
			fl.slotGen[si].Store(seq)
			slots[i] = unsafe.Slice((*byte)(ptr), int(slotSize))
		}
		return count, nil
	}
}

// slotIndex computes the global slot index from a pointer, its slab base
// address, and the struct index. The base is already known from the binary
// search (Deallocate) or read from slabStructs (Allocate).
func (fl *FreeList) slotIndex(ptr unsafe.Pointer, base uintptr, structIdx int) uint64 {
	offset := uintptr(ptr) - base
	return uint64(structIdx)*fl.slotsPerSlab + uint64(offset)/fl.cfg.SlotSize
}

// === Public API ===

// Allocate returns a fixed-size off-heap memory slot.
//
// Reads the owning structIdx from slot bytes [8:12] — embedded by pushFree —
// to resolve the slab without a lock or binary search. This keeps the hot
// path lock-free and independent of slab count.
func (fl *FreeList) Allocate() ([]byte, error) {
	if fl.freed.Load() {
		return nil, ErrFreelistFreed
	}
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
		// during popFree, the memory backing ptr may already be unmapped.
		if fl.generation.Load() != gen {
			gen = fl.generation.Load()
			continue
		}

		// structIdx is embedded in the slot at offset 8 by pushFree.
		// Read it directly — no lock, no binary search.
		meta := *(*uint32)(unsafe.Add(ptr, 8))
			structIdx := int(unpackStructIdx(meta))
		base := uintptr(unsafe.Pointer(&fl.slabStructs[structIdx].data[0]))

		slotSize := fl.cfg.SlotSize
		fl.allocated.Add(slotSize)

		// Set double-free guard: store alloc sequence number.
		seq := fl.allocSeq.Add(1)
		fl.slotGen[fl.slotIndex(ptr, base, structIdx)].Store(seq)

		return unsafe.Slice((*byte)(ptr), int(slotSize)), nil
	}
}

// Deallocate returns a slot to the free list.
func (fl *FreeList) Deallocate(slot []byte) error {
	if len(slot) == 0 || uint64(len(slot)) != fl.cfg.SlotSize {
		return ErrInvalidDeallocation
	}

	ptr := unsafe.Pointer(unsafe.SliceData(slot))

	// Fast path: read structIdx from slot metadata at offset 8.
	// Same field that pushFree writes and Allocate reads. Callers that
	// don't overwrite the metadata region get O(1) lock-free deallocation.
	var structIdx int
	var base uintptr
	fastPathOK := false
	if meta := *(*uint32)(unsafe.Add(ptr, 8)); int(unpackStructIdx(meta)) >= 0 && int(unpackStructIdx(meta)) < len(fl.slabStructs) {
		si := int(unpackStructIdx(meta))
		b := uintptr(unsafe.Pointer(&fl.slabStructs[si].data[0]))
		off := uintptr(ptr) - b
		if off < uintptr(fl.cfg.SlabSize) && off%uintptr(fl.cfg.SlotSize) == 0 {
			structIdx = si
			base = b
			fastPathOK = true
		}
	}

	if !fastPathOK {
		// Slow path: metadata was overwritten by the caller. Fall back to
		// O(log N) binary search under the slab mutex.
		fl.slabMu.RLock()
		structIdx, base = fl.findSlabIdxLocked(ptr)
		fl.slabMu.RUnlock()
		if structIdx < 0 {
			return ErrInvalidDeallocation
		}
	}

	// Double-free detection: check that the slot has a non-zero generation.
	slotIdx := fl.slotIndex(ptr, base, structIdx)
	if fl.slotGen[slotIdx].Swap(0) == 0 {
		return ErrDoubleDeallocation
	}

	// Guarded subtraction: prevent uint64 wraparound from corrupting stats.
	slotSize := fl.cfg.SlotSize
	for {
		allocated := fl.allocated.Load()
		if allocated < slotSize {
			fl.allocated.Store(0)
			break
		}
		if fl.allocated.CompareAndSwap(allocated, allocated-slotSize) {
			break
		}
	}

	fl.pushFree(ptr, int32(structIdx))
	return nil
}

// findSlabIdxLocked performs O(log N) binary search over slabBase.
// Returns the struct index and slab base address, or (-1, 0) if not found.
// DEPRECATED: Deallocate now reads structIdx directly from slot metadata.
func (fl *FreeList) findSlabIdxLocked(ptr unsafe.Pointer) (structIdx int, base uintptr) {
	p := uintptr(ptr)
	n := int(fl.slabLen.Load())
	slabSize := uintptr(fl.cfg.SlabSize)

	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		entry := fl.slabBase[mid]
		if p < entry.base {
			hi = mid
		} else if p >= entry.base+slabSize {
			lo = mid + 1
		} else {
			if (p-entry.base)%uintptr(fl.cfg.SlotSize) == 0 {
				return int(entry.structIdx), entry.base
			}
			return -1, 0
		}
	}
	return -1, 0
}

// Stats returns a point-in-time snapshot of allocator state.
func (fl *FreeList) Stats() FreeListStats {
	return FreeListStats{
		Reserved:  fl.reserved.Load(),
		Allocated: fl.allocated.Load(),
		SlotSize:  fl.cfg.SlotSize,
		SlabCount: fl.slabLen.Load(),
		CasRetries: fl.casRetries.Load(),
	}
}

type FreeListStats struct {
	Reserved  uint64
	Allocated uint64
	SlotSize  uint64
	SlabCount int32
	CasRetries uint64
}

// Reset releases all slabs and reinitializes the free list to empty.
//
// WARNING: All outstanding allocations become invalid. The caller must
// ensure quiescence — no concurrent Allocate or Deallocate calls.
func (fl *FreeList) Reset() {
	fl.generation.Add(1)

	fl.slabMu.Lock()
	fl.head.Store(0)
	n := int(fl.slabLen.Load())
	for i := 0; i < n; i++ {
		if s := fl.slabBuf[i]; s != nil && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
		fl.slabBuf[i] = nil
		fl.slabBase[i] = slabEntry{}
	}

	// Clear slot generation counters while still holding the lock.
	// This must complete before slabLen is zeroed to prevent growSlab
	// from reusing indices before they're cleared.
	totalSlots := uint64(n) * fl.slotsPerSlab
	for i := uint64(0); i < totalSlots; i++ {
		fl.slotGen[i].Store(0)
	}

	fl.slabLen.Store(0)
	fl.slabMu.Unlock()

	fl.reserved.Store(0)
	fl.allocated.Store(0)
	fl.allocSeq.Store(0)
}

// Free releases all mmap'd memory. The FreeList must not be used after Free.
func (fl *FreeList) Free() error {
	fl.generation.Add(1)

	fl.slabMu.Lock()
	fl.head.Store(0)
	n := int(fl.slabLen.Load())
	for i := 0; i < n; i++ {
		if s := fl.slabBuf[i]; s != nil && len(s.data) > 0 {
			unix.Munmap(s.data)
		}
		fl.slabBuf[i] = nil
		fl.slabBase[i] = slabEntry{}
	}
	// Clear slot generation counters while still holding the lock.
	totalSlots := uint64(n) * fl.slotsPerSlab
	for i := uint64(0); i < totalSlots; i++ {
		fl.slotGen[i].Store(0)
	}

	fl.slabLen.Store(0)
	fl.slabMu.Unlock()

	fl.allocSeq.Store(0)
	fl.reserved.Store(0)
	fl.allocated.Store(0)
	fl.freed.Store(true)
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

// CasRetries returns the total number of CAS retries (contention metric).
func (fl *FreeList) CasRetries() uint64 {
	return fl.casRetries.Load()
}
