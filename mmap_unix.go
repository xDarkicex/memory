//go:build !windows

package memory

import "golang.org/x/sys/unix"

func mmapAnonymous(size int) ([]byte, error) {
	return unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
}

func munmap(data []byte) error {
	return unix.Munmap(data)
}
