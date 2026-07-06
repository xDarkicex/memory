//go:build linux
package memory

import (
	"syscall"
)

const (
	MPOL_BIND = 2
)

// mmapRawAnonymous requests page-aligned physical memory directly from the kernel.
// By using syscall.Syscall6, we avoid standard slice headers and force zero-escape.
func mmapRawAnonymous(size uint64) (uintptr, error) {
	addr, _, err := syscall.Syscall6(syscall.SYS_MMAP,
		0, uintptr(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
		0, 0)
	if err != 0 {
		return 0, err
	}
	// TODO: Add SYS_MBIND (mbind) syscall here for strict NUMA socket pinning
	// using the MPOL_BIND constant, ensuring optimal memory bandwidth on multi-socket servers.
	return addr, nil
}

func munmapRaw(addr uintptr, size uint64) error {
	_, _, err := syscall.Syscall(syscall.SYS_MUNMAP, addr, uintptr(size), 0)
	if err != 0 {
		return err
	}
	return nil
}
