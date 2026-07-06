//go:build windows
package memory

import (
	"syscall"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc = kernel32.NewProc("VirtualAlloc")
	procVirtualFree  = kernel32.NewProc("VirtualFree")
)

// mmapRawAnonymous uses VirtualAlloc on Windows to allocate page-aligned off-heap memory.
// It bypasses Cgo completely to maintain zero-allocation constraints.
func mmapRawAnonymous(size uint64) (uintptr, error) {
	// MEM_COMMIT | MEM_RESERVE = 0x1000 | 0x2000
	// PAGE_READWRITE = 0x04
	addr, _, err := procVirtualAlloc.Call(0, uintptr(size), 0x3000, 0x04)
	if addr == 0 {
		return 0, err
	}
	return addr, nil
}

func munmapRaw(addr uintptr, size uint64) error {
	// MEM_RELEASE = 0x8000
	// When using MEM_RELEASE, size must be 0
	r1, _, err := procVirtualFree.Call(addr, 0, 0x8000)
	if r1 == 0 {
		return err
	}
	return nil
}
