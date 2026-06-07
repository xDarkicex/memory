# memory

[![Go Reference](https://pkg.go.dev/badge/github.com/xDarkicex/memory.svg)](https://pkg.go.dev/github.com/xDarkicex/memory)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8.svg?style=flat-square)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/xDarkicex/memory)](https://goreportcard.com/report/github.com/xDarkicex/memory)
[![Test](https://img.shields.io/github/actions/workflow/status/xDarkicex/memory/test.yml?branch=main&style=flat-square)](https://github.com/xDarkicex/memory/actions/workflows/test.yml)
[![Coverage](https://img.shields.io/endpoint?style=flat-square&url=https://raw.githubusercontent.com/xDarkicex/memory/coverage/coverage.json)](https://github.com/xDarkicex/memory/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-MIT-green.svg?style=flat-square)](LICENSE)

Off-heap memory allocators for Go — GC-isolated, lock-free, backed by mmap.

**Production-grade.** Deployed as the allocation layer in the
[OpenClaw memory plugin](https://github.com/xDarkicex/openclaw-memory-libravdb)
vector database daemon, serving embeddings at scale across Linux, macOS, and Windows.
48+ hours of continuous `-race` stress testing with zero crashes, zero data
races, and zero memory leaks.

Package `memory` provides four off-heap allocator types:

- **`Arena`** — variable-size bump-pointer allocator (CAS, lock-free)
- **`Pool`** — variable-size slab allocator (CAS, lock-free, bulk `Reset()`)
- **`FreeList`** — fixed-size lock-free allocator (Treiber stack, per-object `Deallocate()`)
- **`ShardedFreeList`** — sharded fixed-size allocator (per-shard LIFO caches + Hyaline SMR, `Deallocate()` or `Retire()`)

Allocations are served from mmap'd slabs; the Go GC never scans this memory.
Safe memory reclamation (SMR) for concurrent workloads is provided by Hyaline
(PLDI 2021), a reference-counting scheme with a single-store hot path.

## Why use this

- **Off-heap** — allocations live in mmap'd memory, invisible to the Go GC
- **Variable + fixed-size** — `Pool`/`Arena` for arbitrary sizes; `FreeList`/`ShardedFreeList` for fixed-size slots
- **Bulk or per-object free** — `Pool.Reset()` bulk-frees everything; `FreeList.Deallocate()` frees individual slots; `ShardedFreeList.Retire()` defers reclamation via Hyaline SMR
- **Hard memory bounds** — `PoolSize` caps total mmap'd bytes; no unbounded growth
- **Lock-free hot paths** — CAS-based allocation across all allocator types; zero mutex contention on the fast path
- **Zero heap allocations** — verified on every code path with `-benchmem`, escape analysis, and `GODEBUG=gctrace=1`
- **ShardedFreeList with adaptive backpressure** — PI-controlled batch flushing prevents pool exhaustion stalls under extreme oversubscription

## Install

```
go get github.com/xDarkicex/memory
```

## Allocator types

| Type | Allocation model | Free model | Concurrency | Best for |
|------|-----------------|------------|-------------|----------|
| `Pool` | Variable-size (CAS slab) | Bulk `Reset()` | Lock-free multi-producer | Request-scoped scratch buffers, parse buffers |
| `Arena` | Variable-size (CAS bump pointer) | `Reset()` (rewind) or `Free()` (destroy) | Lock-free multi-producer | Frame scratch, per-request temp data |
| `FreeList` | Fixed-size (Treiber stack) | Per-object `Deallocate()` | Lock-free | Fixed-size object pools, per-vector allocations |
| `ShardedFreeList` | Fixed-size (sharded + Hyaline SMR) | Per-object `Deallocate()` or `Retire()` | Lock-free, sharded by goroutine | High-concurrency fixed-size pools, vector DBs |

## Quickstart

### Pool (variable-size, bulk free)

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
defer pool.Free()

buf, err := pool.Allocate(4096) // off-heap, zero GC
// use buf...
pool.Reset() // bulk-free everything
```

### Arena (variable-size, lock-free bump pointer)

```go
arena, err := memory.NewArena(1024 * 1024) // 1MB
ptr, err := arena.Alloc(256)               // bump-pointer, lock-free
arena.Reset()                              // rewind, keep mmap
arena.Free()                               // release mmap
```

### FreeList (fixed-size, per-object free)

```go
fl, err := memory.NewFreeList(memory.FreeListConfig{
    PoolSize:  256 * 1024 * 1024,
    SlotSize:  64,          // every slot is exactly 64 bytes
    SlabSize:  2 * 1024 * 1024,
    SlabCount: 32,
    Prealloc:  true,
})
if err != nil {
    panic(err)
}
defer fl.Free()

slot, err := fl.Allocate()          // returns []byte of exactly SlotSize
fl.Deallocate(slot)                 // return to freelist
fl.BatchAllocate(dst [][]byte)      // batch-refill, amortizes CAS
```

### ShardedFreeList (fixed-size, high concurrency, Hyaline SMR)

```go
sfl, err := memory.NewShardedFreeList(memory.FreeListConfig{
    PoolSize:  256 * 1024 * 1024,
    SlotSize:  64,
    SlabSize:  2 * 1024 * 1024,
    SlabCount: 32,
    Prealloc:  true,
}, 64) // 64 shards
if err != nil {
    panic(err)
}
defer sfl.Free()

slot, err := sfl.Allocate()
// use slot...
sfl.Deallocate(slot) // fast path: shard cache, zero atomics
```

## When to use

- Large, bounded working sets where GC scan time dominates (vector DBs, graph engines, caches)
- You need hard memory limits with no unbounded growth (unlike `sync.Pool`)
- Fixed-size objects with high allocation churn — FreeList/ShardedFreeList are **13-17× faster than `make()`** and off-heap
- Variable-size allocations in scoped lifetimes — Pool/Arena with bulk Reset per request/frame/batch
- Bulk allocation with zero GC pressure — 512MB of off-heap pages, 1M edges at 282ns/slot, GC never scans them
- Cross-goroutine handoff with safe reclamation — ShardedFreeList + Hyaline SMR

## When not to use

- Allocations are tiny (<32B) and ultra-short-lived — Go stack allocation or `sync.Pool` is simpler and the overhead of off-heap management isn't justified
- You need automatic memory management — callers must explicitly `Reset`, `Free`, or `Deallocate`/`Retire`. There is no GC safety net for off-heap memory
- Variable-size per-object free — `Pool` only supports bulk `Reset()`; for per-object free you must use fixed-size allocators (`FreeList` / `ShardedFreeList`)
- The CPU cost of allocation dominates your workload — off-heap allocation is 25× faster per slot and produces zero GC pressure (see Benchmarks), but if allocation speed isn't your bottleneck, the lifecycle management overhead may not be worth it

## Memory Model

All allocations use `unix.Mmap` with `MAP_ANON | MAP_PRIVATE`. This memory is
**not tracked by the Go GC** — no heap scanning, no `GOMEMLIMIT` pressure.
The caller controls the lifecycle.

## API

### Pool

```go
pool, err := memory.NewPool(memory.AllocatorConfig{...})
buf, err := pool.Allocate(size)       // off-heap, 0 heap allocs
stats := pool.Stats()                 // atomic snapshot
pool.Reset()                          // bulk-free, reuse mmap
pool.Free()                           // release mmap, invalidate pool
```

### Arena

```go
arena, err := memory.NewArena(size)
ptr, err := arena.Alloc(size)         // bump-pointer, lock-free
remaining := arena.Remaining()
arena.Reset()                         // rewind, keep mmap
arena.Free()                          // release mmap, invalidate
```

### FreeList

```go
fl, err := memory.NewFreeList(cfg)
slot, err := fl.Allocate()            // single fixed-size slot
n, err := fl.BatchAllocate(dst[:])    // batch refill, amortizes CAS
err := fl.Deallocate(slot)            // return to freelist
stats := fl.Stats()
fl.Reset()                            // bulk-free, reuse mmap
fl.Free()                             // release mmap
```

### ShardedFreeList

```go
sfl, err := memory.NewShardedFreeList(cfg, numShards)
slot, err := sfl.Allocate()           // shard cache → batch refill → global
err := sfl.Deallocate(slot)           // fast path: shard cache (zero atomics)
err := sfl.Retire(slot)               // Hyaline SMR path (see contracts below)
sfl.HyalineEnter(shardIdx)            // protect concurrent readers
sfl.HyalineLeave(shardIdx)            // drain retired nodes, decrement refs
stats := sfl.Stats()
sfl.Reset()                            // bulk-free + restart PID controller
sfl.Free()                             // release mmap + cancel PID controller
```

### Generic helpers

Typed allocation helpers for every allocator — no `unsafe.Pointer` casting
needed. All have `Must` variants that panic instead of returning an error.

```go
// Pool: typed single-value and slice allocation
hdr, err := memory.PoolAlloc[Header](pool)
vec, err := memory.PoolSlice[float32](pool, 1536)
vec = vec[:1536] // reslice to full capacity

// Arena: typed allocation + string + append
hdr, err := memory.ArenaAlloc[Header](arena)
vec, err := memory.ArenaSlice[float32](arena, 1536)
s, err := memory.ArenaNewString(arena, "hello")
vec = memory.ArenaAppend(arena, vec, 1.0, 2.0)

// FreeList: typed alloc + dealloc
hdr, err := memory.FreeListAlloc[Header](fl)
hdr.ID = 42
memory.FreeListDealloc(fl, hdr)
```

| Helper | Allocator | Description |
|--------|-----------|-------------|
| `PoolAlloc[T](pool) *T` | Pool | Allocate a single `T` |
| `PoolSlice[T](pool, n) []T` | Pool | Allocate `[]T` (len=0, cap=n) |
| `MustPoolAlloc[T](pool) *T` | Pool | Panic-on-error variant |
| `MustPoolSlice[T](pool, n) []T` | Pool | Panic-on-error variant |
| `ArenaAlloc[T](arena) *T` | Arena | Allocate a single `T` |
| `ArenaSlice[T](arena, n) []T` | Arena | Allocate `[]T` (len=0, cap=n) |
| `ArenaNewString(arena, s) string` | Arena | Copy a string to the arena |
| `ArenaAppend[T](arena, sl, elems) []T` | Arena | Append to an arena-backed slice |
| `MustArenaAlloc[T](arena) *T` | Arena | Panic-on-error variant |
| `MustArenaSlice[T](arena, n) []T` | Arena | Panic-on-error variant |
| `MustArenaNewString(arena, s) string` | Arena | Panic-on-error variant |
| `FreeListAlloc[T](fl) *T` | FreeList | Allocate a single `T` |
| `FreeListDealloc[T](fl, *T)` | FreeList | Deallocate a `*T` |
| `FreeListSlotFor[T](fl, *T) []byte` | FreeList | Get backing slot for `*T` |
| `MustFreeListAlloc[T](fl) *T` | FreeList | Panic-on-error variant |

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

### Hyaline SMR contracts (ShardedFreeList)

The Hyaline safe memory reclamation protocol has **required invariants**.
Violating any of them causes use-after-free data corruption.

#### Enter/Leave pairing

Every `HyalineEnter` **MUST** be paired with exactly one `HyalineLeave`.

```go
sfl.HyalineEnter(shardIdx)
// ... read shared memory ...
sfl.HyalineLeave(shardIdx) // REQUIRED: paired with Enter
```

#### Retire ordering

`Retire` **MUST NOT** be called while the slot is still reachable by readers
that entered the corresponding Hyaline slot. The correct pattern is:

```go
// CORRECT: unlink from shared structure, then retire
sfl.HyalineEnter(shardIdx)
slot, _ := sfl.Allocate()
// ... use slot, possibly publish it ...
// Remove from shared structure BEFORE retiring
liveMu.Lock()
delete(liveSet, slot)
liveMu.Unlock()
sfl.Retire(slot)       // safe: no reader can reach this slot
sfl.HyalineLeave(shardIdx)
```

```go
// WRONG: retiring while still reachable — reader UAF risk
sfl.HyalineEnter(shardIdx)
sfl.Retire(slot)       // UNSAFE: slot still in liveSet, readers can access it
sfl.HyalineLeave(shardIdx)
```

#### Reader access window

A reader that calls `HyalineEnter` is protected from having memory freed
that was retired *after* the Enter. The reader must obtain its pointers
through a safe publication mechanism (shared slice, map, etc.) and must
not access memory after calling `HyalineLeave`.

```go
// Reader goroutine
sfl.HyalineEnter(shardIdx)
liveMu.RLock()
for _, ptr := range livePtrs {
    _ = *(*uint64)(ptr) // safe: protected by Enter
}
liveMu.RUnlock()
sfl.HyalineLeave(shardIdx)
// UNSAFE to access ptrs after Leave
```

#### Deallocate vs Retire

- **`Deallocate`**: Fast path. Returns the slot directly to the shard cache.
  No SMR protection. Use only when no other goroutine can reach the slot.
- **`Retire`**: Hyaline SMR path. Defers reclamation until all readers that
  entered before the retire have left. Use when concurrent readers may still
  access the slot.

### Double-free detection

Both `Deallocate` and `Retire` detect double-frees via per-slot generation
counters. Attempting to free or retire the same slot twice returns
`ErrDoubleDeallocation`. This is a safety net, not a correctness guarantee
under races — once you deallocate a slot, another goroutine can allocate
and use it before your second deallocate.

### Error semantics

| Error | Meaning |
|-------|---------|
| `ErrInvalidSize` | `size == 0` |
| `ErrPoolExhausted` | `PoolSize` limit reached |
| `ErrMmapFailed` | OS `mmap` call failed (OOM, system limit, hugepage alignment) |
| `ErrArenaExhausted` | Arena has insufficient space |
| `ErrFreelistExhausted` | FreeList pool exhausted (all slots allocated) |
| `ErrInvalidDeallocation` | Slot size mismatch or pointer outside any slab |
| `ErrDoubleDeallocation` | Slot freed or retired twice |
| `ErrLA57` | 5-level paging detected; tagged pointers require ≤48-bit virtual addresses |
| `ErrPoolFreed` | Pool has been freed |
| `ErrFreelistFreed` | FreeList has been freed |
| `ErrArenaCapacityExceeded`| Arena slice capacity exceeded |
| `ErrSlotTooSmall` | Slot size is too small for the requested struct/slice |

## Examples

See [`examples/`](examples/) for runnable demonstrations with benchmarks:

| Example | Scenario | Key metric |
|---|---|---|
| [parser-scratch](examples/parser-scratch/) | JSON tokenizer with scratch buffer | 0 allocs vs 1 heap alloc per parse |
| [request-pool](examples/request-pool/) | Per-request TLV message builder | Bulk `Reset()` vs per-buffer free |
| [vector-storage](examples/vector-storage/) | float32[1536] embeddings off-heap | 0 allocs vs 1 per vector; GC never scans vectors |

Each example includes a `main.go` (runnable demo), `main_test.go` (correctness
tests + benchmarks), and a `README.md` explaining the use case and tradeoffs.

To run an example benchmark:

```
go test -bench=. -benchmem ./examples/parser-scratch/
```

## Benchmarks

See [BENCHMARK.md](BENCHMARK.md) for extended methodology, raw data, and
historical trends. Summary below. Apple M2, Go 1.25, Darwin (arm64). All paths
show **0 heap allocations**.

### Per-vector allocation (1536 float32 = 6KB, best-of-3)

| Allocator | ns/op | B/op | allocs/op | vs `make()` |
|-----------|-------|------|-----------|-------------|
| **FreeList** | **30.8** | 0 | 0 | **17.0× faster** |
| **ShardedFreeList** | **38.5** | 0 | 0 | **13.6× faster** |
| Slabby | 63.4 | 0 | 0 | 8.3× faster |
| `make([]float32, 1536)` | 525 | 6,144 | 1 | 1.00× baseline |
| Pool (CAS slab) | 1,041 | 0 | 0 | 2.0× slower |

Pool is variable-size (CAS slab) — it's the wrong tool for a fixed-size 6KB allocation. Included for completeness.

### memory vs `make()` — hot path

| Allocator | ns/op | B/op | allocs/op | vs `make()` |
|-----------|-------|------|-----------|-------------|
| **FreeList** | **30.2** | 0 | 0 | **25.8× faster** |
| **ShardedFreeList** | **38.6** | 0 | 0 | **20.2× faster** |
| `make([]float32, 1536)` | 779 | 6,144 | 1 | 1.00× baseline |

Per-vector allocation (1536 float32 = 6KB). The Go heap path allocates 6KB and the GC
scans it. FreeList/ShardedFreeList return an off-heap slot in 30-39ns with zero GC impact.

### memory vs `make()` — bulk

| Workload | `make()` | memory | Delta |
|----------|----------|--------|-------|
| Index build (10K vectors, seq) | 11.9ms, 61.7MB heap | 12.3ms (Pool), **13.8KB heap** | same speed, **4,500× less heap** |
| Concurrent build (8 workers) | 3.1ms, 61.7MB heap | 4.6ms (FreeList), **362KB heap** | 1.5× slower, **170× less heap** |
| Concurrent query (8 workers) | 3.4ms | 3.4-3.6ms (all allocators) | within noise, all zero heap hot path |

The throughput is comparable for compute-bound workloads (cosine search dominates).
The win is GC isolation — 13KB vs 62MB of heap tracked by the GC. On a 10K-vector
index, that's the difference between the GC scanning nothing and scanning 62MB.

### RAG workload: index build (10K vectors, sequential)

B/op and allocs/op reflect scaffolding (pool creation, goroutines), not the allocation hot path.

| Allocator | ms/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| `make()` (Go heap) | 11.9 | 61,685,782 | 10,001 |
| Pool | 12.3 | 13,813 | 8 |
| FreeList | 13.3 | 361,308 | 8 |
| ShardedFreeList | 14.5 | 376,134 | 17 |
| Slabby | 26.0 | 62,221,757 | 10,024 |

### RAG workload: concurrent query (8 goroutines, top-10 cosine)

All allocators show the same scaffolding overhead (~292 B/op, 3 allocs/op). The allocation hot path is zero heap.

| Allocator | ms/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| Pool | 3.41 | 292 | 3 |
| `make()` (Go heap) | 3.42 | 292 | 3 |
| FreeList | 3.45 | 292 | 3 |
| ShardedFreeList | 3.61 | 292 | 3 |
| Slabby | 3.70 | 292 | 3 |

### ShardedFreeList stress hammer (256 goroutines, 256 shards, 128MB pool)

| Duration | Total ops | ops/sec | Errors | Error rate | Stalls | Corruption |
|----------|-----------|---------|--------|-----------|--------|-----------|
| 30s | 0.43B | 14.43M | 1.39M | 0.32% | 0 | 0 |
| 5m | 3.95B | 13.16M | 4.13M | 0.10% | 0 | 0 |
| 10m | 7.34B | 12.23M | 2.22M | 0.03% | 0 | 0 |
| **1h** | **42.02B** | **11.67M** | **15.59M** | **0.037%** | **0** | **0** |

**1-hour post-hammer recovery:** 10,000/10,000 alloc/free cycles succeeded.
RSS flat at ~6 MB (128 MB pool is off-heap mmap). Zero memory leak, zero
throughput degradation beyond asymptotic PID settling. **v1.0.0-gold certified.**

### Before vs. after: static threshold → PID adaptive threshold (5-minute run)

| Metric | Static (threshold=65) | PID (adaptive) | Improvement |
|--------|----------------------|----------------|-------------|
| Stall duration | **6 seconds** | **0 seconds** | Eliminated |
| Error rate | 1.07% | 0.10% | **10× lower** |
| Total errors | 40.1M | 4.13M | **89.7% reduction** |

### memory vs slabby — bulk graph workloads

Head-to-head with [slabby](https://github.com/xDarkicex/slabby) (`sync.Pool`-based
off-heap allocator) on graph-engine workloads. All runs: 0 heap allocs on the hot
path for memory; slabby hits full heap fallback on bulk workloads when the pool
exhausts.

| Workload | slabby | memory | Delta |
|----------|--------|--------|-------|
| BFS buffer pool (get/put, sequential) | 1,027 ns | **46.5 ns** | **22× faster** |
| BFS buffer pool (get/put, 8 concurrent) | 294 ns | **134 ns** | **2.2× faster** |
| Edge bulk alloc (1M edges, 80B slots) | 223s, 110MB heap, 1M allocs | **282ms, 24MB heap, 19 allocs** | **792× faster** |
| Page table bulk (128K pages, 4KB slots) | 29.8s, 16.8MB heap, 131K allocs | **147ms, 3.2MB heap, 16 allocs** | **203× faster** |
| Concurrent AddEdge (8 workers, 1M edges) | 273s, 108MB heap, 1M allocs | **301ms, 24MB heap, 54 allocs** | **907× faster** |

slabby's pool saturates under bulk allocation; every subsequent allocation falls
through to the Go heap. memory never hits this cliff — all slot data stays off-heap
(mmap'd), invisible to the GC. The 24MB in memory's bulk column is the
`make([][]byte, N)` slice header array, not the slot data itself.

### Pool allocation paths

| Path | ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| Hot path (slab has space) | 124M | 9.4 | 0 | 0 |
| Slow path (scan for free slab) | 3.7M | 314 | 0 | 0 |
| Grow path (mmap new slab) | 1.9M | 620 | 0 | 0 |
| Large allocation (1MB, direct mmap) | 2.0M | 595 | 0 | 0 |

### Reset cost (Pool)

| Slabs | ns/op | B/op | allocs/op |
|-------|-------|------|-----------|
| 4 | 2,339 | 0 | 0 |
| 16 | 9,463 | 0 | 0 |
| 64 | 39,591 | 0 | 0 |
| 256 | 172,423 | 0 | 0 |

### GC Isolation (`GODEBUG=gctrace=1`)

Sustained runs under `GODEBUG=gctrace=1`. Every path shows **`0→0→0 MB`**
live heap with zero automatic GC triggers.

| Path | Duration | GC Cycles | Live Heap | Auto GC |
|------|----------|-----------|-----------|---------|
| Pool hot path | 10s | 7 forced | 0→0→0 MB | 0 |
| Pool grow path | 5s | 4 forced | 0→0→0 MB | 0 |
| Pool large allocation | 5s | 4 forced | 0→0→0 MB | 0 |
| FreeList per-vector alloc+free | 1s | 2 forced | 0→0→0 MB | 0 |
| ShardedFreeList per-vector alloc+free | 1s | 2 forced | 0→0→0 MB | 0 |
| ShardedFreeList + PID controller | 60m | all forced | 0→0→0 MB | 0 |

gctrace format (`live_before→live_marked→live_after`): all zeros means the GC
found nothing to scan. All cycles are `(forced)` — triggered by `runtime.GC()`
in benchmark scaffolding, not by heap pressure. No automatic GC fired because
the runtime never detected heap growth.

The PID controller (100ms ticker, per-vector allocations, 1-hour stress hammer)
adds zero measurable heap pressure. GC trace shows steady `0→0→0 MB` with no
creep over time.

### Platform notes

RSS behavior after `Reset()` varies by platform:

| Platform | `madvise` behavior | RSS after Reset |
|----------|-------------------|-----------------|
| Linux | `MADV_DONTNEED` releases pages immediately | RSS drops |
| macOS (darwin) | `MADV_FREE` lazily reclaims pages | RSS may linger until pressure |

On macOS, `top`/`htop` may show higher resident memory after `Reset()` due to
lazy page reclamation. This is cosmetic — the OS reclaims pages under pressure.
Go runtime metrics (`MemStats`) always report zero heap growth.

## Configuration reference

### AllocatorConfig (Pool)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `PoolSize` | uint64 | 64MB | Hard limit on total mmap'd bytes |
| `SlabSize` | uint64 | 1MB | Size of each slab |
| `SlabCount` | int | 16 | Initial slab descriptor capacity |
| `Prealloc` | bool | false | Eagerly allocate `SlabCount` slabs at creation |
| `UseHugePages` | bool | false | Use `MAP_HUGETLB` (Linux only; requires 2MB-aligned `SlabSize`) |

### FreeListConfig (FreeList / ShardedFreeList)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `PoolSize` | uint64 | 64MB | Hard limit on total mmap'd bytes |
| `SlotSize` | uint64 | 64 | Fixed size of each slot (min 32 for metadata) |
| `SlabSize` | uint64 | 1MB | Size of each slab |
| `SlabCount` | int | 16 | Initial slab descriptor capacity |
| `Prealloc` | bool | false | Eagerly allocate `SlabCount` slabs at creation |

**Prealloc:** When true, `NewPool`/`NewFreeList` eagerly allocates `SlabCount`
slabs. On failure, already-allocated slabs are rolled back and `ErrMmapFailed`
is returned.

**UseHugePages:** Linux only. Attempts `MAP_HUGETLB`; silently falls back to
regular mmap if unavailable. macOS ignores this flag.

**PoolSize** is a hard limit on mmap'd bytes tracked via atomic `reserve()`.
When exhausted, `Allocate` returns `ErrPoolExhausted`.

**SlotSize** (FreeList/ShardedFreeList): Must be ≥ 32 bytes. The slot metadata
(Hyaline chain pointers, batch references, struct index, shard index) occupies
offsets 0–31. Offsets 32+ are usable payload.

### ShardedFreeList shard count

The `numShards` parameter to `NewShardedFreeList` defaults to 64. It is rounded
up to the next power of two. More shards reduce cross-CPU contention but increase
memory overhead (per-shard batch, caches, mutex). 64 is a good default for most
workloads; 256 is appropriate for extreme oversubscription scenarios.

For P-bound affinity (goroutines pinned to OS threads), build with `-tags procpin`
to use `runtime.procPin` instead of stack-address hashing for shard selection.

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

`memory.Hint(HintWillNeed, ptr, len)` or `memory.Hint(HintDontNeed, ptr, len)` wraps `madvise(2)` for
cache warming or page reclaim hints. Linux uses `MADV_DONTNEED` (eager);
macOS uses `MADV_FREE` (lazy).

### Performance characteristics

| Operation | Complexity | Locks |
|-----------|------------|-------|
| Pool hot path (slab has space) | O(1), lock-free CAS | None |
| Pool slow path (scan slabs) | O(n slabs) | None |
| FreeList.Allocate | O(1), lock-free CAS | None |
| ShardedFreeList.Allocate (cache hit) | O(1), zero atomics | None |
| ShardedFreeList.Allocate (batch refill) | O(1), lock-free CAS | None |
| ShardedFreeList.Retire | O(1) amortized, lock-free CAS | `batchMu` (per-shard, uncontended) |
| HyalineEnter | O(1), single atomic store | None |
| HyalineLeave | O(nodes in slot chain) | None |
| PID controller | O(1) every 100ms, background | None |
| Reset | O(n slabs) munmap | None |

### PID adaptive threshold (ShardedFreeList)

`NewShardedFreeList` launches a background PI controller (Kp=2.0, Ki=0.5,
anti-windup ±100, 100ms ticker) that dynamically adjusts the Hyaline batch
flush threshold from its default of 65 down to as low as 1. When the pool
drops below 20% free capacity, the controller forces partial batches to
flush sooner, preventing the exhaustion cliff that occurs with a static
threshold. The hot path (`hyalineRetire`) sees only a single
`atomic.Uint64.Load` — zero additional contention or branching.

The controller is automatically restarted on `Reset()` and cancelled on
`Free()`.

### Watchdog

A process-wide heap pressure monitor is available via
`RegisterMemoryPressureCallback(threshold, fn)`. It monitors Go heap metrics
(`HeapInuse`), not the off-heap mmap'd memory managed by this package.

## Testing & Correctness

### Coverage

**90.8% statement coverage** on the main package. Remaining uncovered branches
are impractical guard clauses (`math.MaxInt` overflow on exabyte allocations,
mmap/munmap OS-level error paths not mockable without function-pointer
indirection) and non-deterministic Hyaline race-condition edges.

Zero 0%-coverage functions remain in the main package. All four allocator types,
all helper APIs, the watchdog, Hyaline SMR, shard caches, and the PID
controller have full coverage.

### Race Detector

Every test passes `go test -race -count=10` — zero races, zero panics. The race
detector found and was used to fix:
- Non-atomic generation counter reads across all allocator types
- `shardNode.slabID`/`next` field races
- `Clear` vs `Report` on `sync.Map` observer
- `notifyAllocate` observer bool race

### Stress Testing

All tests stressed at 16 parallel via `golang.org/x/tools/cmd/stress`:

| Test group | Runs | Failures |
|-----------|------|----------|
| FreeList accessors + error paths | 5,516 | 0 |
| ShardedFreeList error/edge paths | 10,123 | 0 |
| Pool/Arena + property tests | 1,499 | 0 |
| **Total** | **17,138** | **0** |

Full stress-hammer certification runs at 1-hour, 5-minute, and 30-second
durations are documented in [BENCHMARK.md §5.4](#54--hyaline-smr--adaptive-pid-threshold-tier-2-fix).

### Extended endurance run

A continuous 48-hour stress hammer was run with the Go race detector enabled
(`-race`, Google ThreadSanitizer) and 32× worker oversubscription (32
goroutines per CPU core). The race detector slows execution by 5–10× and
tracks every memory access, placing the allocator under sustained extreme
pressure.

| Metric | Result |
|--------|--------|
| Duration | **48+ hours** |
| Worker oversubscription | 32× per core |
| Race detector | enabled (`-race`) |
| Sustained CPU | 662% (6.6 cores) |
| Crashes | **0** |
| Data races | **0** |
| Memory leaks | **0** |
| Post-hammer recovery | 10,000/10,000 alloc/free cycles — **clean** |

The run was terminated manually (not by failure). No stalls, no corruption,
no pool exhaustion under the most adversarial conditions the allocator
supports.

## What This Is NOT

- **Not GC-safe** — memory is not zeroed on alloc/reset; caller manages contents
- **Not thread-safe for `Arena` Reset** — single-producer reset only; calling Reset concurrently with Alloc causes overlapping allocations
- **Not a drop-in for `make()` or `sync.Pool`** — designed for explicit lifecycle control (Reset/Free/Retire). Per-allocation speed is often faster (13-17× for fixed-size, 2-22× vs [`slabby`](https://github.com/xDarkicex/slabby) for buffers, 200-900× for bulk), but you must manage lifetimes
- **Not safe for use-after-Reset** — accessing an allocation after `Reset()` will segfault or corrupt data
- **Not safe for use-after-Retire without Enter** — accessing a retired slot without holding an active Hyaline enter is a use-after-free bug

## Theoretical Foundations

This implementation bridges high-level Go concurrency with low-level systems research:

- **Safe Memory Reclamation**: Based on *Hyaline: Fast and Transparent Lock-Free Memory Reclamation* (PLDI '21) by Nikolaev and Ravindran. This provides $O(1)$ reclamation and robustness against stalled goroutines, enabling our 13.8M ops/sec throughput without the frequent memory barrier overhead inherent to traditional *Hazard Pointers* (Michael, 2004).
- **Lock-Free Primitives**: Utilizes a sharded *Treiber Stack* (1986). To resolve the ABA problem (a classic weakness of Treiber stacks in non-GC languages), 16-bit generation tags are packed into 48-bit virtual addresses. Furthermore, sharding is used to avoid the scalability bottlenecks of global stacks, a principle outlined in *A Scalable Lock-free Stack Algorithm* (Hendler, Shavit, and Yerushalmi, 2004).
- **Adaptive Control**: Reclamation pressure is managed via a PID controller, dynamically tuning batch flush thresholds to prevent liveness stalls under extreme oversubscription, applying principles from *Feedback Control for Computer Systems* (Janert).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
