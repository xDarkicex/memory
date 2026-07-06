package memory

import _ "unsafe"

// Spin performs a CPU-level pipeline yield for exponential backoff during CAS contention.
//
//go:noescape
//go:nosplit
func Spin()

// CpuPause is an explicit pipeline pause instruction.
//
//go:noescape
//go:nosplit
func CpuPause()
