//go:build windows

package memory

import "unsafe"

func init() {
	// Windows uses regular VirtualAlloc-backed pages only.
	HugepageSize = 0
}

func (p *Pool) mmapSlab(slabSize uint64) ([]byte, error) {
	return p.mmapSlabBase(slabSize)
}

func (fl *FreeList) mmapSlab(slabSize uint64) ([]byte, error) {
	return fl.mmapSlabBase(slabSize)
}

func Hint(h MemoryHint, ptr unsafe.Pointer, length int) {
}
