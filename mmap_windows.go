//go:build windows

package memory

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func mmapAnonymous(size int) ([]byte, error) {
	addr, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

func munmap(data []byte) error {
	return windows.VirtualFree(uintptr(unsafe.Pointer(unsafe.SliceData(data))), 0, windows.MEM_RELEASE)
}
