//go:build darwin

package memory

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmapSlab on Darwin ignores UseHugePages (MAP_HUGETLB is Linux-only).
// Always uses regular mmap; huge page allocation is silently ignored.
func (p *Pool) mmapSlab(slabSize uint64) ([]byte, error) {
	return p.mmapSlabBase(slabSize)
}

// mmapSlab on Darwin always uses regular mmap (no huge page support).
func (fl *FreeList) mmapSlab(slabSize uint64) ([]byte, error) {
	return fl.mmapSlabBase(slabSize)
}

// Hint passes madvise hints to the Darwin kernel.
//
// Platform divergence: Darwin maps HintDontNeed to MADV_FREE (lazy reclaim
// under memory pressure, pages may retain content until reclaimed). Linux
// maps HintDontNeed to MADV_DONTNEED (eager page discard, next access faults
// to zero). Callers requiring deterministic zeroing after HintDontNeed must
// call ZeroMemory explicitly.
func Hint(h MemoryHint, ptr unsafe.Pointer, length int) {
	if length <= 0 {
		return
	}
	var advice int
	switch h {
	case HintWillNeed:
		advice = unix.MADV_WILLNEED
	case HintDontNeed:
		advice = unix.MADV_FREE
	default:
		advice = unix.MADV_NORMAL
	}
	pageSize := uintptr(PageSize)
	pageOffset := uintptr(ptr) % pageSize
	pageBase := unsafe.Add(ptr, -int(pageOffset))
	pageLen := (pageOffset + uintptr(length) + pageSize - 1) &^ (pageSize - 1)
	_ = unix.Madvise(unsafe.Slice((*byte)(pageBase), pageLen), advice)
}
