//go:build !linux && !windows && !darwin && !freebsd && !openbsd && !netbsd
package memory

import "errors"

// Fallback for unsupported OS where raw SYS_MMAP is unavailable.
// We return an error explicitly, as this memory package strictly requires zero-allocation off-heap capabilities.

func mmapRawAnonymous(size uint64) (uintptr, error) {
	return 0, errors.New("zero-allocation off-heap mapping not supported on this OS")
}

func munmapRaw(addr uintptr, size uint64) error {
	return errors.New("zero-allocation off-heap unmapping not supported on this OS")
}

// MmapAnonymous is not supported on this platform.
func MmapAnonymous(_ int) ([]byte, error) {
	return nil, errors.New("MmapAnonymous not supported on this OS")
}
