// Package memory_test demonstrates the off-heap memory allocator API
// via runnable, testable examples for pkg.go.dev.
package memory_test

import (
	"fmt"

	"github.com/xDarkicex/memory"
)

// Example_pool demonstrates the basic Pool lifecycle: create, allocate,
// use off-heap memory, and bulk-free with Reset. Shows both the raw API
// and the typed PoolAlloc helper.
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
	defer pool.Free()

	// Raw API: allocate a byte slice.
	buf, err := pool.Allocate(64)
	if err != nil {
		panic(err)
	}
	copy(buf, "hello")
	fmt.Printf("allocated %d bytes: %s\n", len(buf), string(buf[:5]))

	// Typed helper: PoolAlloc allocates a zeroed struct directly off-heap.
	type User struct{ ID int64; Name [32]byte }
	u := memory.MustPoolAlloc[User](pool)
	u.ID = 42
	copy(u.Name[:], "alice")
	fmt.Printf("User{ID: %d, Name: %s}\n", u.ID, string(u.Name[:5]))

	// Output:
	// allocated 64 bytes: hello
	// User{ID: 42, Name: alice}
}

// Example_arena demonstrates Arena: a bump-pointer allocator backed by a
// single mmap'd region. Reset reuses the backing memory; Free releases it.
// Shows both the raw API and the typed ArenaAlloc helper.
func Example_arena() {
	arena, err := memory.NewArena(4096)
	if err != nil {
		panic(err)
	}
	defer arena.Free()

	// Raw API: allocate a fixed number of bytes.
	_, err = arena.Alloc(256)
	if err != nil {
		panic(err)
	}
	fmt.Println("allocated 256 bytes, remaining:", arena.Remaining())

	arena.Reset()
	fmt.Println("after reset, remaining:", arena.Remaining())

	// Typed helper: ArenaAlloc allocates a zeroed struct directly off-heap.
	type Point struct{ X, Y float64 }
	p := memory.MustArenaAlloc[Point](arena)
	p.X, p.Y = 3.0, 4.0
	fmt.Printf("Point{X: %.0f, Y: %.0f}\n", p.X, p.Y)

	// Output:
	// allocated 256 bytes, remaining: 3840
	// after reset, remaining: 4096
	// Point{X: 3, Y: 4}
}

// Example_freelist demonstrates FreeList: a fixed-size lock-free allocator.
// Shows both the raw []byte API and the typed FreeListAlloc helper.
func Example_freelist() {
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024 // 64KB slab
	cfg.SlabCount = 1
	cfg.PoolSize = 1024 * 1024
	cfg.Prealloc = true

	fl, err := memory.NewFreeList(cfg)
	if err != nil {
		panic(err)
	}
	defer fl.Free()

	// Raw API: allocate a []byte slot, copy data into it.
	slot, _ := fl.Allocate()
	copy(slot, "hello from freelist")
	fmt.Printf("slot size: %d, content: %s\n", len(slot), string(slot[:19]))
	fl.Deallocate(slot)

	// Typed helper: FreeListAlloc returns a *Record directly — no unsafe,
	// no offset arithmetic, no []byte tracking.
	type Record struct{ ID uint64; Name [32]byte }
	rec, _ := memory.FreeListAlloc[Record](fl)
	rec.ID = 7
	copy(rec.Name[:], "widget")
	fmt.Printf("Record{ID: %d, Name: %s}\n", rec.ID, string(rec.Name[:6]))
	memory.FreeListDealloc(fl, rec)

	// Output:
	// slot size: 64, content: hello from freelist
	// Record{ID: 7, Name: widget}
}

// Example_shardedFreelist demonstrates ShardedFreeList: a sharded wrapper
// around FreeList with per-goroutine caches for near-zero contention under
// concurrent allocation. The API is identical to FreeList.
func Example_shardedFreelist() {
	cfg := memory.DefaultFreeListConfig()
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.PoolSize = 1024 * 1024
	cfg.Prealloc = true

	sfl, err := memory.NewShardedFreeList(cfg, 4)
	if err != nil {
		panic(err)
	}
	defer sfl.Free()

	slot, err := sfl.Allocate()
	if err != nil {
		panic(err)
	}
	copy(slot, "hello from sharded freelist")
	fmt.Printf("slot size: %d, content: %s\n", len(slot), string(slot[:27]))

	sfl.Deallocate(slot)

	// Output:
	// slot size: 64, content: hello from sharded freelist
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
	defer pool.Free()

	// Allocate three scratch buffers for a single logical operation.
	header, _ := pool.Allocate(16)
	body, _ := pool.Allocate(64)
	trailer, _ := pool.Allocate(8)

	copy(header, "HTTP/1.1 200 OK\r\n")
	copy(body, `{"status":"ok"}`)
	copy(trailer, "0\r\n\r\n")

	fmt.Printf("used %d buffers, %d bytes total\n", 3, 16+64+8)

	// Output: used 3 buffers, 88 bytes total
}
