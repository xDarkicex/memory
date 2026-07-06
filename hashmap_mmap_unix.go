//go:build darwin || freebsd || openbsd || netbsd
package memory

import (
	"syscall"
)

// mmapRawAnonymous requests page-aligned physical memory directly from the kernel
// without allocating a Go slice header, completely blinding the GC.
func mmapRawAnonymous(size uint64) (uintptr, error) {
	addr, _, err := syscall.Syscall6(syscall.SYS_MMAP,
		0, uintptr(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
		0, 0)
	if err != 0 {
		return 0, err
	}
	return addr, nil
}

func munmapRaw(addr uintptr, size uint64) error {
	_, _, err := syscall.Syscall(syscall.SYS_MUNMAP, addr, uintptr(size), 0)
	if err != 0 {
		return err
	}
	return nil
}
