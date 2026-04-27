# Request Pool

**Use case:** Web servers, RPC handlers, and connection handlers that allocate per-request scratch buffers, serialization state, and temporary objects — all freed together after the response.

## The Problem

`sync.Pool` is the standard Go approach for buffer reuse, but has fundamental limitations:

```go
var bufPool = sync.Pool{New: func() any { return make([]byte, 4096) }}

func handler(w http.ResponseWriter, r *http.Request) {
    buf := bufPool.Get().([]byte)
    defer bufPool.Put(buf[:0]) // must return individually
    // ...use buf...
}
```

- **No bulk operations** — each buffer must be `Put()` individually. With 20+ pooled objects per request, this is 20+ pool operations.
- **GC clears pools** — every GC cycle evicts idle items from `sync.Pool`, forcing re-allocation.
- **No memory bounds** — `sync.Pool` has no limit; under burst load it can allocate unbounded memory.
- **Pinning risks** — keeping a pooled object too long prevents other goroutines from using it.

Direct heap allocation without a pool shows the per-request cost clearly:

```go
func handler(w http.ResponseWriter, r *http.Request) {
    buf := make([]byte, 0, 4096) // heap alloc every request
    // ...build response...
    // buf escapes → GC must collect it
}
```

## The Arena Solution

One allocation. One free. Zero GC.

```go
pool, _ := memory.NewPool(memory.AllocatorConfig{...})

func handler(w http.ResponseWriter, r *http.Request) {
    buf, _ := pool.Allocate(4096)  // off-heap
    // ...use buf for headers, body, serialization...
    pool.Reset() // bulk-free everything
}
```

## Benchmark

| Approach | ns/op | B/op | allocs/op |
|---|---|---|---|
| Arena request pool | 2080 | 0 | 0 |
| Standard (heap) | 363 | 4096 | 1 |

The arena path is consistently zero-alloc regardless of concurrency or GC state — all data lives in mmap'd memory. The standard path heap-allocates 4KB per request, which the GC must scan and collect.

**Tradeoff:** Arena allocation has higher per-call overhead (mmap bookkeeping + CAS) compared to a bump-pointer heap alloc. The win comes at scale: bounded memory, zero GC scanning of request data, and bulk `Reset()` instead of N individual `Put()` calls. When a single request allocates 10+ separate buffers (headers, body, trailers, serialization buffers), `pool.Reset()` replaces 10 `sync.Pool.Put()` calls.
