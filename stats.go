// Package memory — statistics and diagnostics.
//
// Provides GC stats, memory profiles, platform hints, and ZeroMemory
// for explicit memory clearing. All read functions take atomic snapshots.

package memory

import (
	"runtime"
	"time"
	"unsafe"
)

// MemoryHint provides hints to the memory system.
type MemoryHint int

const (
	HintNormal MemoryHint = iota
	HintWillNeed
	HintDontNeed
)

// Hint is defined in memory_linux.go and memory_darwin.go based on platform.

// GCStats holds garbage collector statistics.
type GCStats struct {
	PauseTotal time.Duration
	PauseLast  time.Duration
	NumGC      uint32
	Forced     bool
}

// ReadGCStats reads current GC statistics using NumForcedGC.
func ReadGCStats() GCStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return GCStats{
		PauseTotal: time.Duration(m.PauseTotalNs),
		PauseLast:  time.Duration(m.PauseNs[m.NumGC%256]),
		NumGC:      m.NumGC,
		Forced:     m.NumForcedGC > 0,
	}
}

// Profile records memory profile data.
type Profile struct {
	Alloc      uint64
	TotalAlloc uint64
	Sys        uint64
	Lookups    uint64
	Mallocs    uint64
	Frees      uint64
}

// ReadProfile reads current memory profile.
func ReadProfile() Profile {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Profile{
		Alloc:      m.Alloc,
		TotalAlloc: m.TotalAlloc,
		Sys:        m.Sys,
		Lookups:    m.Lookups,
		Mallocs:    m.Mallocs,
		Frees:      m.Frees,
	}
}

// ZeroMemory securely zeros a memory region.
func ZeroMemory(p unsafe.Pointer, n uintptr) {
	if n > 0 {
		clear(unsafe.Slice((*byte)(p), n))
	}
}

// MemStats provides system memory statistics.
type MemStats struct {
	Total     uint64
	Available uint64
	Used      uint64
	Free      uint64
	SwapTotal uint64
	SwapUsed  uint64
	Cached    uint64
	Buffers   uint64
}

// ReadMemStats reads Go heap memory statistics.
// Note: this reports Go runtime heap metrics, not physical RAM.
// For off-heap mmap'd memory managed by this allocator, look at PoolStats.
func ReadMemStats() MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return MemStats{
		Total:     m.HeapSys,     // Total memory obtained from OS
		Available: m.HeapSys,    // Total available (same as Total for heap)
		Used:      m.HeapInuse,  // In-use by runtime allocator
		Free:      m.HeapIdle,   // Memory not used by runtime
		SwapTotal: 0,
		SwapUsed:  0,
		Cached:    m.HeapReleased,
		Buffers:   0,
	}
}
