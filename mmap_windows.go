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

func MmapFileReadOnly(fd int, offset int64, size int) ([]byte, error) {
	return mmapFile(fd, offset, size, false)
}

func MmapFile(fd int, offset int64, size int, writable bool) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	if offset < 0 {
		return nil, windows.ERROR_INVALID_PARAMETER
	}

	h := windows.Handle(fd)
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &d); err != nil {
		return nil, err
	}
	fileSize := (int64(d.FileSizeHigh) << 32) | int64(d.FileSizeLow)
	if offset+int64(size) > fileSize {
		return nil, windows.ERROR_INVALID_PARAMETER
	}

	var allocInfo windows.SystemInfo
	windows.GetSystemInfo(&allocInfo)
	align := int64(allocInfo.AllocationGranularity)
	alignOffset := offset & ^(align - 1)
	diff := int(offset - alignOffset)

	protect := uint32(windows.PAGE_READONLY)
	mapAccess := uint32(windows.FILE_MAP_READ)
	if writable {
		protect = windows.PAGE_READWRITE
		mapAccess = windows.FILE_MAP_WRITE
	}

	maxSizeHigh := uint32((alignOffset + int64(diff+size)) >> 32)
	maxSizeLow := uint32((alignOffset + int64(diff+size)) & 0xFFFFFFFF)

	mapping, err := windows.CreateFileMapping(h, nil, protect, maxSizeHigh, maxSizeLow, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(mapping)

	offsetHigh := uint32(alignOffset >> 32)
	offsetLow := uint32(alignOffset & 0xFFFFFFFF)

	addr, err := windows.MapViewOfFile(mapping, mapAccess, offsetHigh, offsetLow, uintptr(diff+size))
	if err != nil {
		return nil, err
	}

	data := unsafe.Slice((*byte)(unsafe.Pointer(addr)), diff+size)
	return data[diff:], nil
}

func Munmap(data []byte) error {
	if len(data) == 0 && cap(data) == 0 {
		return nil
	}
	ptr := uintptr(unsafe.Pointer(unsafe.SliceData(data)))
	var allocInfo windows.SystemInfo
	windows.GetSystemInfo(&allocInfo)
	align := uintptr(allocInfo.AllocationGranularity)
	alignPtr := ptr & ^(align - 1)
	return windows.UnmapViewOfFile(alignPtr)
}
