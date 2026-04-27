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

// Hint passes madvise hints to the Darwin kernel.
// MADV_FREE is used in place of MADV_DONTNEED: pages are lazily reclaimable
// under memory pressure but NOT immediately zeroed. Callers requiring
// guaranteed zeroing after HintDontNeed must call ZeroMemory explicitly.
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
	pageBase := (uintptr(ptr) / pageSize) * pageSize
	offset := uintptr(ptr) - pageBase
	pageLen := (offset + uintptr(length) + pageSize - 1) &^ (pageSize - 1)
	_ = unix.Madvise(unsafe.Slice((*byte)(unsafe.Pointer(pageBase)), pageLen), advice)
}
