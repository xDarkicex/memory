//go:build !windows

package memory

import (
	"golang.org/x/sys/unix"
	"unsafe"
)

func mmapAnonymous(size int) ([]byte, error) {
	return unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
}

func MmapFileReadOnly(fd int, offset int64, size int) ([]byte, error) {
	return MmapFile(fd, offset, size, false)
}

func MmapFile(fd int, offset int64, size int, writable bool) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	if offset < 0 {
		return nil, unix.EINVAL
	}
	
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if offset+int64(size) > stat.Size {
		return nil, unix.EINVAL
	}

	pageSize := int64(unix.Getpagesize())
	alignOffset := offset & ^(pageSize - 1)
	diff := int(offset - alignOffset)
	
	prot := unix.PROT_READ
	if writable {
		prot |= unix.PROT_WRITE
	}
	
	data, err := unix.Mmap(fd, alignOffset, diff+size, prot, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	
	return data[diff:], nil
}

func Munmap(data []byte) error {
	if len(data) == 0 && cap(data) == 0 {
		return nil
	}
	ptr := uintptr(unsafe.Pointer(unsafe.SliceData(data)))
	pageSize := uintptr(unix.Getpagesize())
	alignPtr := ptr & ^(pageSize - 1)
	diff := ptr - alignPtr
	
	baseSlice := unsafe.Slice((*byte)(unsafe.Pointer(alignPtr)), int(diff)+cap(data))
	return unix.Munmap(baseSlice)
}

// Keep munmap for internal package use
func munmap(data []byte) error {
	return Munmap(data)
}
