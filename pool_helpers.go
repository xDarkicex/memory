// Package memory — generic helpers for off-heap typed allocation via Pool.
//
// These helpers wrap Pool.Allocate with compile-time type safety, matching
// the same pattern as the Arena helpers. The returned pointers and slices
// reference mmap'd memory that is invisible to the Go GC.
//
// Unlike Arena, Pool supports concurrent multi-producer allocation. No
// individual Deallocate is needed — call Pool.Free() or Pool.Reset() to
// release everything at once.

package memory

import "unsafe"

// PoolAlloc allocates a zeroed T from the pool and returns *T.
// The pointer is invalid after Pool.Free or Pool.Reset.
//
// Example:
//
//	vec, err := PoolAlloc[struct{ X, Y, Z float64 }](pool)
//	if err != nil { ... }
//	vec.X, vec.Y, vec.Z = 1, 2, 3
func PoolAlloc[T any](pool *Pool) (*T, error) {
	var zero T
	buf, err := pool.Allocate(uint64(unsafe.Sizeof(zero)))
	if err != nil {
		return nil, err
	}
	return (*T)(unsafe.Pointer(unsafe.SliceData(buf))), nil
}

// MustPoolAlloc is PoolAlloc but panics on error. Use in initialization
// paths where allocation failure is fatal.
func MustPoolAlloc[T any](pool *Pool) *T {
	p, err := PoolAlloc[T](pool)
	if err != nil {
		panic(err)
	}
	return p
}

// PoolSlice allocates a backing array of cap T from the pool and returns a
// slice with len=0, cap=cap. append works normally until capacity is
// exhausted, at which point Go falls back to the heap.
//
// Example:
//
//	ids, err := PoolSlice[int64](pool, 256)
//	if err != nil { ... }
//	ids = append(ids, 1, 2, 3) // stays off-heap (cap=256)
func PoolSlice[T any](pool *Pool, cap int) ([]T, error) {
	if cap == 0 {
		return nil, nil
	}
	var zero T
	sz := unsafe.Sizeof(zero) * uintptr(cap)
	buf, err := pool.Allocate(uint64(sz))
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*T)(unsafe.Pointer(unsafe.SliceData(buf))), cap)[:0], nil
}

// MustPoolSlice is PoolSlice but panics on error.
func MustPoolSlice[T any](pool *Pool, cap int) []T {
	s, err := PoolSlice[T](pool, cap)
	if err != nil {
		panic(err)
	}
	return s
}
