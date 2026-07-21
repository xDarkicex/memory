// Package memory — generic helpers for off-heap typed allocation via Arena.
//
// These helpers wrap Arena.Alloc with compile-time type safety. They eliminate
// manual unsafe.Sizeof arithmetic and unsafe.Pointer casting. The returned
// pointers and slices reference mmap'd memory that is invisible to the Go GC.
//
// Every helper has two forms:
//   - ArenaAlloc[T] returns (*T, error) — caller handles exhaustion gracefully.
//   - MustArenaAlloc[T] returns *T — panics on error, for init paths.
//
// Sharp edge: T must not contain Go-managed pointer types (pointers, slices,
// maps, interfaces, channels, strings) unless the referent also lives in arena
// memory. A Go pointer in mmap'd memory creates a GC reachability gap — the
// GC cannot see the pointer, so the referent may be collected.

package memory

import "unsafe"

// ArenaAlloc allocates a zeroed T from the arena and returns *T. It clears the
// allocation even after Arena.Reset, whose deliberately O(1) rewind leaves
// old bytes in place. Use ArenaAllocUninitialized only when every field will
// be overwritten before its first read.
// The pointer is invalid after Arena.Reset or Arena.Free.
//
// Example:
//
//	cat, err := ArenaAlloc[struct{ Name [32]byte; Age int }](arena)
//	if err != nil { ... }
//	copy(cat.Name[:], "Whiskers")
//	cat.Age = 3
func ArenaAlloc[T any](arena *Arena) (*T, error) {
	p, err := ArenaAllocUninitialized[T](arena)
	if err != nil {
		return nil, err
	}
	var zero T
	*p = zero
	return p, nil
}

// ArenaAllocUninitialized allocates a T without clearing it. This is the raw,
// highest-throughput form for callers that fully initialize the allocation.
// Bytes may contain data from an earlier Arena.Reset generation.
func ArenaAllocUninitialized[T any](arena *Arena) (*T, error) {
	var typed *T
	ptr, err := arena.Alloc(uint64(unsafe.Sizeof(*typed)))
	if err != nil {
		return nil, err
	}
	return (*T)(ptr), nil
}

// MustArenaAlloc is ArenaAlloc but panics on error. Use in initialization
// paths where allocation failure is fatal.
func MustArenaAlloc[T any](arena *Arena) *T {
	p, err := ArenaAlloc[T](arena)
	if err != nil {
		panic(err)
	}
	return p
}

// MustArenaAllocUninitialized is the panic-on-exhaustion form of
// ArenaAllocUninitialized.
func MustArenaAllocUninitialized[T any](arena *Arena) *T {
	p, err := ArenaAllocUninitialized[T](arena)
	if err != nil {
		panic(err)
	}
	return p
}

// ArenaSlice allocates a zeroed backing array of cap T from the arena and
// returns a slice with len=0, cap=cap. It clears the allocation on every call,
// including after Arena.Reset. append works normally until capacity is
// exhausted, at which point Go falls back to the heap. Use [ArenaAppend] for
// arena-guaranteed append that panics on overflow.
//
// Example:
//
//	toys, err := ArenaSlice[Toy](arena, 16)
//	if err != nil { ... }
//	toys = append(toys, Toy{Name: "bone"}) // stays in arena (cap=16)
func ArenaSlice[T any](arena *Arena, capacity int) ([]T, error) {
	values, err := ArenaSliceUninitialized[T](arena, capacity)
	if err != nil {
		return nil, err
	}
	clear(values[:cap(values)])
	return values, nil
}

// ArenaSliceUninitialized allocates a backing array without clearing it. The
// returned slice has len=0 and cap=cap. It is intended for dense buffers that
// are completely overwritten before read; after Arena.Reset, its backing
// bytes retain prior contents.
func ArenaSliceUninitialized[T any](arena *Arena, capacity int) ([]T, error) {
	if capacity == 0 {
		return nil, nil
	}
	var typed *T
	sz := unsafe.Sizeof(*typed) * uintptr(capacity)
	ptr, err := arena.Alloc(uint64(sz))
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*T)(ptr), capacity)[:0], nil
}

// MustArenaSlice is ArenaSlice but panics on error.
func MustArenaSlice[T any](arena *Arena, capacity int) []T {
	s, err := ArenaSlice[T](arena, capacity)
	if err != nil {
		panic(err)
	}
	return s
}

// MustArenaSliceUninitialized is the panic-on-exhaustion form of
// ArenaSliceUninitialized.
func MustArenaSliceUninitialized[T any](arena *Arena, capacity int) []T {
	s, err := ArenaSliceUninitialized[T](arena, capacity)
	if err != nil {
		panic(err)
	}
	return s
}

// ArenaNewString copies s into an arena-backed buffer and returns a string
// pointing into the arena. The string header is a value type — it can
// live in a struct field off-heap, and the GC will trace the header
// but the backing data is in mmap'd memory (no GC scan needed).
//
// Example:
//
//	type Dog struct{ Name string }
//	dog, _ := MustArenaAlloc[Dog](arena)
//	dog.Name = MustArenaNewString(arena, "Rex")
func ArenaNewString(arena *Arena, s string) (string, error) {
	if len(s) == 0 {
		return "", nil
	}
	ptr, err := arena.Alloc(uint64(len(s)))
	if err != nil {
		return "", err
	}
	dst := unsafe.Slice((*byte)(ptr), len(s))
	copy(dst, s)
	return string(dst), nil
}

// MustArenaNewString is ArenaNewString but panics on error.
func MustArenaNewString(arena *Arena, s string) string {
	str, err := ArenaNewString(arena, s)
	if err != nil {
		panic(err)
	}
	return str
}

// ArenaAppend appends elems to slice, panicking if the result would exceed
// cap. The panic value is [ErrArenaCapacityExceeded] so callers can use
// errors.Is in recover. This guarantees the backing store stays in arena
// memory. Use with [ArenaSlice] for Odin-style arena-bounded dynamic arrays.
//
// Example:
//
//	toys := MustArenaSlice[Toy](arena, 4)
//	toys = ArenaAppend(arena, toys, Toy{"bone"}, Toy{"ball"})
//	toys = ArenaAppend(arena, toys, Toy{"stick"}) // panics if len exceeds 4
func ArenaAppend[T any](arena *Arena, slice []T, elems ...T) []T {
	if len(slice)+len(elems) > cap(slice) {
		panic(ErrArenaCapacityExceeded)
	}
	return append(slice, elems...)
}
