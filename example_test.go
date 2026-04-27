// Package memory_test demonstrates the off-heap memory allocator API
// via runnable, testable examples for pkg.go.dev.
package memory_test

import (
	"fmt"

	"github.com/xDarkicex/memory"
)

// Example_pool demonstrates the basic Pool lifecycle: create, allocate,
// use off-heap memory, and bulk-free with Reset.
func Example_pool() {
	cfg := memory.AllocatorConfig{
		PoolSize:  1024 * 1024, // 1MB
		SlabSize:  64 * 1024,   // 64KB slabs
		SlabCount: 1,
		Prealloc:  true,
	}
	pool, err := memory.NewPool(cfg)
	if err != nil {
		panic(err)
	}
	defer pool.Reset()

	buf, err := pool.Allocate(64)
	if err != nil {
		panic(err)
	}
	copy(buf, "hello")
	fmt.Printf("allocated %d bytes: %s\n", len(buf), string(buf[:5]))
	pool.Reset()

	// Output: allocated 64 bytes: hello
}

// Example_arena demonstrates Arena: a bump-pointer allocator backed by a
// single mmap'd region. Reset reuses the backing memory; Free releases it.
func Example_arena() {
	arena, err := memory.NewArena(4096)
	if err != nil {
		panic(err)
	}
	defer arena.Free()

	_, err = arena.Alloc(256)
	if err != nil {
		panic(err)
	}
	fmt.Println("allocated 256 bytes, remaining:", arena.Remaining())

	arena.Reset()
	fmt.Println("after reset, remaining:", arena.Remaining())

	// Output:
	// allocated 256 bytes, remaining: 3840
	// after reset, remaining: 4096
}

// Example_poolScoped demonstrates the bulk-free pattern: allocate multiple
// buffers for a logical scope (request, frame, batch), then free them all
// with a single Reset call.
func Example_poolScoped() {
	cfg := memory.AllocatorConfig{
		PoolSize:  1024 * 1024, // 1MB
		SlabSize:  64 * 1024,   // 64KB slabs
		SlabCount: 2,
		Prealloc:  true,
	}
	pool, err := memory.NewPool(cfg)
	if err != nil {
		panic(err)
	}
	defer pool.Reset()

	// Allocate three scratch buffers for a single logical operation.
	header, _ := pool.Allocate(16)
	body, _ := pool.Allocate(64)
	trailer, _ := pool.Allocate(8)

	copy(header, "HTTP/1.1 200 OK\r\n")
	copy(body, `{"status":"ok"}`)
	copy(trailer, "0\r\n\r\n")

	fmt.Printf("used %d buffers, %d bytes total\n", 3, 16+64+8)
	pool.Reset()

	// Output: used 3 buffers, 88 bytes total
}
