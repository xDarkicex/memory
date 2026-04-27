# memory

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarkicex/memory.svg)](https://pkg.go.dev/github.com/xDarkicex/memory)
[![CI](https://github.com/xDarkicex/memory/actions/workflows/test.yml/badge.svg)](https://github.com/xDarkicex/memory/actions/workflows/test.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/xDarkicex/memory)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Off-heap memory allocator for Go — GC-isolated slabs backed by mmap.

Package `memory` provides an off-heap slab allocator for Go programs with
large bounded working sets where GC scan cost dominates latency. Allocations
are served from mmap'd slabs via a lock-free CAS hot path and freed in bulk
with a single `Reset()` call. The Go GC never scans this memory.

## Why use this

- **Off-heap** — allocations live in mmap'd memory, invisible to the Go GC
- **Bulk free** — one `Reset()` releases everything; no per-object cleanup
- **Hard memory bounds** — `PoolSize` caps total mmap'd bytes; no unbounded growth
- **Lock-free hot path** — typical allocations served via CAS, no mutex contention
- **Zero heap allocations** — verified on every code path with `-benchmem`, escape analysis, and `GODEBUG=gctrace=1`

## Install

```
go get github.com/xDarkicex/memory
```

## Quickstart

```go
pool, err := memory.NewPool(memory.AllocatorConfig{
    PoolSize:  64 * 1024 * 1024, // 64MB hard limit
    SlabSize:  1024 * 1024,      // 1MB slabs
    SlabCount: 16,
    Prealloc:  true,
})
if err != nil {
    panic(err)
}
defer pool.Reset()

buf, err := pool.Allocate(4096) // off-heap, zero GC
if err != nil {
    panic(err)
}
// use buf...
pool.Reset() // bulk-free everything
```

## When to use

- Large, bounded working sets (vector DBs, caches, parse buffers)
- GC scan time dominates latency percentiles
- Hard memory limits needed (no unbounded growth like `sync.Pool`)
- Allocation lifetimes are naturally scoped (per-request, per-frame, per-batch)
- You accept trading per-allocation speed for zero GC overhead

## When not to use

- Allocations are small and short-lived (Go's stack allocator is faster)
- You need automatic memory management (no manual `Reset`)
- Your working set fits comfortably in the Go heap with acceptable GC pauses
- You need per-allocation free (arena model only supports bulk free)
- You're building a library that can't impose lifecycle rules on callers

## Memory Model

All allocations use `unix.Mmap` with `MAP_ANON | MAP_PRIVATE`. This memory is
**not tracked by the Go GC** — no heap scanning, no `GOMEMLIMIT` pressure.
The caller controls the lifecycle: all memory lives until `Pool.Reset()` or
`Arena.Free()` releases it.

## API

### Pool

`Pool` is a concurrent slab allocator. Small allocations (≤ `SlabSize`) are
served from slabs via lock-free CAS. Large allocations (> `SlabSize`) get a
dedicated mmap'd region tracked for cleanup. All are freed together with `Reset()`.

```go
pool, err := memory.NewPool(memory.AllocatorConfig{
    PoolSize:  64 * 1024 * 1024, // 64MB hard limit
    SlabSize:  1024 * 1024,      // 1MB slabs
    SlabCount: 16,
    Prealloc:  true,
})
if err != nil {
    panic(err)
}
defer pool.Reset()

buf, err := pool.Allocate(4096) // off-heap, 0 heap allocs
stats := pool.Stats()           // atomic snapshot
pool.Reset()                    // bulk-free everything
```

### Arena

`Arena` is a bump-pointer allocator backed by a single mmap'd region.
Best for single-producer, short-lived allocation bursts where the caller
controls the full lifecycle. `Reset()` reuses the backing memory; `Free()`
releases it.

```go
arena, err := memory.NewArena(1024 * 1024) // 1MB
ptr, err := arena.Alloc(256)               // bump-pointer, lock-free
remaining := arena.Remaining()
arena.Reset()                              // rewind, keep mmap
arena.Free()                               // release mmap, invalidate
```

### Pool vs Arena

| | Pool | Arena |
|---|---|---|
| Concurrency | Multi-producer safe | Single-producer |
| Allocation | Slab allocator (CAS) | Bump pointer (CAS) |
| Free | Bulk `Reset()` | `Reset()` (reuse) or `Free()` (destroy) |
| Large allocs | Yes (> SlabSize, separate mmap) | No (bounded by arena size) |
| Use case | Shared request pools, caches, vector stores | Frame scratch, per-request temp data |

## Safety

### Reset contract

**Reading or writing through an allocation after `Reset()` is undefined
behavior** — it will either segfault (if the OS has reclaimed the page) or
silently corrupt data (if the page has been re-mmap'd and handed to another
allocation). The caller is responsible for ensuring no references survive
`Reset()`.

Calling `Reset()` while other goroutines hold allocations from the same pool
is also undefined behavior. The caller must ensure quiescence — no in-flight
`Allocate` calls — before calling `Reset()`.

### Generation counter

`Reset()` increments a generation counter before unmapping slabs. Allocators
check the generation before and after their CAS: if the generation changed,
the allocation is retried rather than returning a pointer into memory being
unmapped. **This is best-effort, not a true RCU barrier.** The only guarantee
is external quiescence.

### Error semantics

| Error | Meaning |
|-------|---------|
| `ErrInvalidSize` | `size == 0` |
| `ErrPoolExhausted` | `PoolSize` limit reached or `Prealloc` exceeds `PoolSize` |
| `ErrMmapFailed` | OS `mmap` call failed (OOM, system limit, hugepage alignment) |
| `ErrArenaExhausted` | Arena has insufficient space |

## Examples

See [`examples/`](examples/) for runnable demonstrations with benchmarks:

| Example | Scenario | Arena vs std |
|---|---|---|
| [parser-scratch](examples/parser-scratch/) | JSON tokenizer with scratch buffer | 0 allocs vs 1 heap alloc per parse (4KB) |
| [request-pool](examples/request-pool/) | Per-request TLV message builder | Bulk `Reset()` vs per-buffer free; 0 allocs vs 1 |
| [vector-storage](examples/vector-storage/) | float32[1536] embeddings off-heap | 0 allocs vs 1 per vector (6KB); GC never scans vectors |

Each example includes a `main.go` (runnable demo), `main_test.go` (correctness
tests + benchmarks), and a `README.md` explaining the use case and tradeoffs.

To run an example benchmark:
```
go test -bench=. -benchmem ./examples/parser-scratch/
```

## Benchmarks

Apple M2, Go 1.25, Darwin (arm64). All paths show **0 heap allocations**.
Hot path is ~9.4 ns/op; off paths (slow, grow, large) stay sub-microsecond.

### Allocation paths

| Path | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Hot path (64B, slab has space) | 124M | 9.4 | 0 | 0 |
| Slow path (scan for free slab) | 3.7M | 314 | 0 | 0 |
| Grow path (mmap new slab) | 1.9M | 620 | 0 | 0 |
| Large allocation (1MB, direct mmap) | 2.0M | 595 | 0 | 0 |
| Varied sizes (16–4096B) | 100M | 11.5 | 0 | 0 |

### Pool vs Arena (64B allocation)

| Allocator | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Pool.Allocate | 126M | 9.4 | 0 | 0 |
| Arena.Alloc | 131M | 8.8 | 0 | 0 |

### Reset cost

| Slabs | ns/op | B/op | allocs/op |
|---|---|---|---|
| 4 | 2,339 | 0 | 0 |
| 16 | 9,463 | 0 | 0 |
| 64 | 39,591 | 0 | 0 |
| 256 | 172,423 | 0 | 0 |

### Concurrent (8 goroutines)

| Benchmark | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Per-goroutine pool | 79M | 14.9 | 0 | 0 |
| Shared pool | 10.6M | 107 | 4 | 0¹ |

¹ 4 B/op is `sync.WaitGroup` stack spill in benchmark scaffolding, not a heap allocation.

### GC Isolation (`GODEBUG=gctrace=1`)

Sustained runs under `GODEBUG=gctrace=1`. Every path shows **`0→0→0 MB`**
live heap with zero automatic GC triggers.

| Path | Duration | GC Cycles | Live Heap | Auto GC |
|---|---|---|---|---|
| Hot path | 10s | 7 forced | 0→0→0 MB | 0 |
| Grow path | 5s | 4 forced | 0→0→0 MB | 0 |
| Large allocation | 5s | 4 forced | 0→0→0 MB | 0 |

gctrace format (`live_before→live_marked→live_after`): all zeros means the GC
found nothing to scan. All cycles are `(forced)` — triggered by `runtime.GC()`
in benchmark scaffolding, not by heap pressure. No automatic GC fired because
the runtime never detected heap growth.

### Platform notes

RSS behavior after `Reset()` varies by platform:

| Platform | `madvise` behavior | RSS after Reset |
|---|---|---|
| Linux | `MADV_DONTNEED` releases pages immediately | RSS drops |
| macOS (darwin) | `MADV_FREE` lazily reclaims pages | RSS may linger until pressure |

On macOS, `top`/`htop` may show higher resident memory after `Reset()` due to
lazy page reclamation. This is cosmetic — the OS reclaims pages under pressure.
Go runtime metrics (`MemStats`) always report zero heap growth.

## Configuration reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `PoolSize` | uint64 | 64MB | Hard limit on total mmap'd bytes |
| `SlabSize` | uint64 | 1MB | Size of each slab |
| `SlabCount` | int | 16 | Initial slab descriptor capacity |
| `Prealloc` | bool | false | Eagerly allocate `SlabCount` slabs at creation |
| `UseHugePages` | bool | false | Use `MAP_HUGETLB` (Linux only; requires 2MB-aligned `SlabSize`) |

**Prealloc:** When true, `NewPool` eagerly allocates `SlabCount` slabs. On
failure, already-allocated slabs are rolled back and `ErrMmapFailed` is returned.

**UseHugePages:** Linux only. Attempts `MAP_HUGETLB`; silently falls back to
regular mmap if unavailable. macOS ignores this flag.

**PoolSize** is a hard limit on mmap'd bytes tracked via atomic `reserve()`.
When exhausted, `Allocate` returns `ErrPoolExhausted`.

## Reference

### Stats

```go
stats := pool.Stats() // atomic snapshot, safe for concurrent use
// PoolStats{Reserved, Allocated, Committed, PeakUsage, SlabCount, SlabSize, Align}
```

- `Reserved` = total bytes mmap'd (≤ `PoolSize`)
- `Allocated` = bytes handed to callers (may slightly overcount during Reset churn)
- `Committed` = bytes mmap'd for large (>SlabSize) allocations

### Alignment

All allocations are **8-byte aligned** for SIMD/ARM compatibility.

### Memory hints

`memory.Hint(HintWillNeed | HintDontNeed, ptr, len)` wraps `madvise(2)` for
cache warming or page reclaim hints. Linux uses `MADV_DONTNEED` (eager);
macOS uses `MADV_FREE` (lazy).

### Performance characteristics

| Operation | Complexity | Locks |
|-----------|------------|-------|
| Hot path (slab has space) | O(1), lock-free CAS | None |
| Slow path (scan slabs) | O(n slabs) | None |
| New slab creation | O(1) + mmap | None |
| Large allocation | O(1) + mmap | `largeMu` (brief) |
| Reset | O(n slabs) munmap | `largeMu` (brief) |

### Watchdog

A process-wide heap pressure monitor is available via
`RegisterMemoryPressureCallback(threshold, fn)`. It monitors Go heap metrics
(`HeapInuse`), not the off-heap mmap'd memory managed by this package.

## What This Is NOT

- **Not GC-safe** — memory is not zeroed on alloc/reset; caller manages contents
- **Not thread-safe for `Arena`** — single-producer bump allocator; concurrent use causes corruption
- **Not a substitute for `sync.Pool`** — designed for explicit lifecycle control, not automatic GC integration
- **Not a general-purpose allocator** — tuned for slab workloads; large allocations bypass slabs
- **Not safe for use-after-Reset** — accessing an allocation after `Reset()` will segfault or corrupt data

## License

MIT — see [LICENSE](LICENSE).
