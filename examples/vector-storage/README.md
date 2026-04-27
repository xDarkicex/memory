# Vector Storage

**Use case:** Storing millions of embedding vectors (float32[1536]) off-heap so the GC never scans them.

## The Problem

A vector database with 1M embeddings at 1536 dimensions:

```go
// 1M × 1536 × 4 bytes = 6.14 GB of float32s on the GC heap
vectors := make([][]float32, 1_000_000)
for i := range vectors {
    vectors[i] = make([]float32, 1536) // 6KB heap alloc × 1M
}
```

The GC must **scan all 6GB** during mark phase, even though embeddings are long-lived and never freed individually. This causes:
- GC cycle times proportional to heap size (seconds per cycle)
- STW pauses growing linearly with live set
- GOMEMLIMIT pressure forcing premature GC

## The Arena Solution

Embeddings live in mmap'd memory — invisible to the GC. Only the 24-byte slice header touches the Go heap.

```go
pool, _ := memory.NewPool(memory.AllocatorConfig{PoolSize: 8 * 1024 * 1024 * 1024}) // 8GB

data, _ := pool.Allocate(1536 * 4) // off-heap
vec := unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), 1536)
// vec points to mmap'd memory — GC won't scan it
```

## Benchmark

| Approach | ns/op | B/op | allocs/op |
|---|---|---|---|
| Arena vector store | 2608 | 0 | 0 |
| Standard (heap) | 415 | 6144 | 1 |
| Arena cosine search | 1771601 | 0 | 0 |
| Standard cosine search | 1756699 | 0 | 0 |

Vector storage with the arena shows **0 B/op, 0 allocs/op** — every vector allocation returns mmap'd memory without touching the Go heap. The standard version allocates 6KB per vector on the heap.

Cosine similarity search is allocation-free for both approaches (it only reads existing vectors), with identical performance — proving the arena has no read-side overhead.

**Tradeoff:** Arena vector allocation is slower per-call (2608 vs 415 ns) but eliminates all GC scanning of vector data. For a vector DB serving queries, the dominant cost is similarity search (1.7ms per 1000-vector scan), not vector allocation. The arena's real value is keeping 6GB+ of embeddings off the GC heap, ensuring GC cycle times stay in microseconds instead of seconds.
