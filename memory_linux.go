//go:build linux

package memory

import (
	"golang.org/x/sys/unix"
	"unsafe"
)

const (
	MAP_HUGETLB   = 0x40000
	MADV_HUGEPAGE = 14
	MADV_FREE     = 8
)

// mmapSlab on Linux attempts huge page allocation when UseHugePages is enabled.
// Falls back to regular mmap if huge pages are unavailable.
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
// Applies MADV_HUGEPAGE when slab size >= HugepageSize to opt into
// transparent huge page promotion opportunistically (no privileges required).
func (p *Pool) mmapSlabRegular(slabSize uint64) ([]byte, error) {
	data, err := p.mmapSlabBase(slabSize)
	if err != nil {
		return nil, err
	}
	// Request THP promotion for slabs >= HugepageSize. The kernel promotes
	// 2MB-aligned regions opportunistically; ignored silently if THP is disabled.
	if slabSize >= HugepageSize {
		_ = unix.Madvise(data, MADV_HUGEPAGE)
	}
	return data, nil
}

// Hint passes madvise hints to the Linux kernel.
// MADV_DONTNEED is eager: the kernel reclaims pages immediately and
// re-faults them as zero on next access. For guaranteed zeroing after
// a HintDontNeed, callers must call ZeroMemory explicitly.
func Hint(h MemoryHint, ptr unsafe.Pointer, length int) {
	if length <= 0 {
		return
	}
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
	pageOffset := uintptr(ptr) % pageSize
	pageBase := unsafe.Add(ptr, -int(pageOffset))
	pageLen := (pageOffset + uintptr(length) + pageSize - 1) &^ (pageSize - 1)
	_ = unix.Madvise(unsafe.Slice((*byte)(pageBase), pageLen), advice)
}

// HintFreeLinux advises the kernel that the given region can be lazily
// reclaimed under memory pressure without immediate zeroing on next access.
// Unlike MADV_DONTNEED (eager discard), pages are only reclaimed if needed
// and retain their content until actually reclaimed. For slab memory that
// will be reused soon, MADV_FREE avoids unnecessary refault churn.
func HintFreeLinux(ptr unsafe.Pointer, length int) {
	if length <= 0 {
		return
	}
	pageSize := uintptr(PageSize)
	pageOffset := uintptr(ptr) % pageSize
	pageBase := unsafe.Add(ptr, -int(pageOffset))
	pageLen := (pageOffset + uintptr(length) + pageSize - 1) &^ (pageSize - 1)
	_ = unix.Madvise(unsafe.Slice((*byte)(pageBase), pageLen), MADV_FREE)
}
