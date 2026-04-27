# Off-Heap Memory Allocator

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Odin's `arena.Allocator` for Go** — explicit, scoped, zero-GC, bulk-free.

Lock-free slab allocator backed by `mmap`. Provides GC-isolated, off-heap memory for high-throughput, low-latency workloads. Modeled after [Odin's arena allocator](https://odin-lang.org/docs/overview/#allocators): allocate freely, free all at once with `Reset()`. Use it anywhere you'd use `mem.Arena` in Odin — frame scratch, request pools, parser temp — not where you'd use the default context allocator.

## Memory Model

All allocations use `unix.Mmap` with `MAP_ANON | MAP_PRIVATE`. This memory:

- Is **not tracked by Go's GC** — no heap scanning, no GOMEMLIMIT pressure
- Lives outside the Go heap — accessible via `unsafe.Pointer` without GC interference
- Is **manually managed** — caller controls lifecycle via `Pool.Reset()` or `Arena.Free()`

## Pool

`Pool` is a lock-free slab allocator. Memory is pre-allocated in slabs; small allocations are served from existing slabs using CAS, avoiding per-allocation syscalls.

### Allocation Sizes

| Size | Path |
|------|------|
| `size == 0` | Returns `(nil, ErrInvalidSize)` |
| `size > SlabSize` | Direct mmap — tracked in `large` list for Reset cleanup |
| `size <= SlabSize` | Slab allocation — served from hot slab or slow-path scan |

### Configuration

```go
cfg := memory.AllocatorConfig{
    PoolSize:     64 * 1024 * 1024, // Hard limit on mmap'd bytes
    SlabSize:     1024 * 1024,       // 1MB slabs (default)
    SlabCount:    16,                 // Pre-allocated slab descriptors
    Prealloc:     false,             // Eagerly allocate slabs at creation
    UseHugePages: false,             // Use MAP_HUGETLB (Linux, requires root)
}
pool, err := memory.NewPool(cfg)
```

### Prealloc

When `Prealloc: true`, `NewPool` eagerly allocates `SlabCount` slabs immediately:

- Validates `SlabCount * SlabSize <= PoolSize` before allocation
- On failure, all successfully allocated slabs aremunmap'd and `reserved` is rolled back
- Sets `cursor = 0` so first slab is hot immediately

### UseHugePages

When `UseHugePages: true` (Linux only):

- Attempts `mmap` with `MAP_HUGETLB` flag (2MB huge pages on x86_64)
- **Requires `SlabSize` to be a multiple of `HugepageSize` (2MB)** — validated at pool creation
- On failure (no root, no hugepages available), silently falls back to regular `mmap`
- Silent fallback is intentional — guarantees pool creation succeeds even without hugepage support

### PoolSize Enforcement

`PoolSize` is a **hard limit** on bytes mmap'd via atomic `reserve()`. When exhausted, `Allocate` returns `ErrPoolExhausted` even if existing slabs have free space.

**Note:** `PoolSize` tracks mmap'd bytes (`reserved`), not allocated bytes.

## Arena

Bump-pointer arena for short-lived allocations. Single-threaded only.

```go
arena, err := memory.NewArena(1024 * 1024) // 1MB arena
ptr, err := arena.Alloc(256)
remaining := arena.Remaining()
arena.Reset()  // Reset offset, reuse backing (mmap preserved)
arena.Free()   // Destructor — invalidates arena (mmap released)
```

## Memory Contract

### Caller Responsibilities

1. **No concurrent `Reset` + `Allocate`**

   Calling `Reset()` while other goroutines hold allocations from the pool is **undefined behavior** (SIGSEGV or silent corruption).

   **Contract:** Caller must ensure quiescence — no in-flight `Allocate` calls — before calling `Reset()`.

2. **Pool lifetime > all allocations**

   Allocations remain valid until `Pool.Reset()`. Calling `Reset` invalidates all outstanding allocations.

3. **Arena single-threaded**

   `Arena.Alloc` is a single-producer bump allocator. Concurrent `Alloc` calls on the same arena cause overlapping allocations. `Arena.Reset()` is also single-producer only.

### Reset Safety

`Reset()` uses a **generation counter** as best-effort protection:

- Increments `generation` before unmapping
- Allocators check generation before and after CAS
- If generation changed, allocation is retried rather than returning a pointer to unmapped memory

**Generation checks are best-effort, not a true RCU barrier.** The only guarantee is external quiescence. Violating the "no concurrent Reset+Allocate" contract can result in segfault.

### Error Semantics

| Error | Meaning |
|-------|---------|
| `ErrInvalidSize` | `size == 0` |
| `ErrPoolExhausted` | `PoolSize` limit reached, or `Prealloc` exceeds `PoolSize` |
| `ErrMmapFailed` | OS `mmap` call failed (OOM, system limit, or hugepage alignment violation) |
| `ErrArenaExhausted` | Arena has insufficient space |

## Stats Semantics

```go
type PoolStats struct {
    Reserved  uint64 // Total bytes mmap'd (pool limit ceiling)
    Allocated uint64 // Bytes handed out to callers
    Committed uint64 // Bytes mmap'd for large (>SlabSize) allocations
    PeakUsage uint64 // Peak single large allocation size
    SlabCount int32  // Number of slabs
    SlabSize  uint64 // Per-slab size
    Align     uint64 // Alignment (always 8)
}
```

- `Reserved == Allocated + free space in slabs + fragmentation`
- `Committed ⊆ Reserved` (large allocations only)
- `Allocated` may slightly **overcount** during heavy Reset churn — conservative is safer for monitoring than undercounting

## Alignment

All allocations are **8-byte aligned** for SIMD/ARM compatibility.

## Watchdog

Process-wide memory pressure monitor. Monitors **Go heap metrics** (`HeapInuse`), not system RSS.

```go
stop := memory.RegisterMemoryPressureCallback(
    512*1024*1024, // 512MB threshold
    func(stats memory.MemStats) {
        log.Printf("heap pressure: %d bytes", stats.Used)
    },
)
defer stop()
```

For system-level memory monitoring, implement a custom callback using `unix.Sysinfo`.

## Performance Characteristics

| Operation | Complexity | Locks |
|-----------|------------|-------|
| Hot path (slab has space) | O(1), lock-free CAS | None |
| Slow path (scan slabs) | O(n slabs) | None |
| New slab creation | O(1) + mmap | None |
| Large allocation | O(1) + mmap | `largeMu` (brief) |
| Reset | O(n slabs) munmap | `largeMu` (brief) |

The hot path has no locks and typically hits the first slab. Slow path adds O(n) where n = number of slabs.

## Configuration Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `PoolSize` | uint64 | 64MB | Hard limit on mmap'd bytes |
| `SlabSize` | uint64 | 1MB | Size of each slab |
| `SlabCount` | int | 16 | Initial slab descriptor capacity |
| `Prealloc` | bool | false | Eagerly allocate `SlabCount` slabs at creation |
| `UseHugePages` | bool | false | Use `MAP_HUGETLB` (Linux, requires 2MB-aligned `SlabSize`) |

## Benchmarks

Apple M2, Go 1.25, Darwin (arm64). All allocation paths are **zero heap allocs**.

### Pool Allocate (64 bytes, hot path)

| Benchmark | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Hot path (prealloc) | 124M | 9.9 | 0 | 0 |
| Hot path (lazy) | 121M | 10.0 | 0 | 0 |
| Varied sizes (16–4096) | 100M | 12.0 | 0 | 0 |
| Concurrent (per-goroutine) | 77M | 15.5 | 0 | 0 |

### Pool vs Arena

| Benchmark | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Pool.Allocate (64B) | 128M | 9.5 | 0 | 0 |
| Arena.Alloc (64B) | 157M | 7.7 | 0 | 0 |

### Slow / Grow / Large paths

| Benchmark | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Slow path (scan slabs) | 3.7M | 333 | 0 | 0 |
| Grow path (new slab) | 2.0M | 616 | 0 | 0 |
| Large allocation (1MB) | 1.9M | 684 | 69¹ | 1¹ |

¹ Large allocations (>SlabSize) use one heap-allocated slab descriptor for `large` list tracking. Rare path.

### Reset cost

| Slabs | ns/op | B/op | allocs/op |
|---|---|---|---|
| 4 | 2,341 | 0 | 0 |
| 16 | 9,696 | 0 | 0 |
| 64 | 40,423 | 0 | 0 |
| 256 | 179,396 | 12 | 0 |

### Concurrent (shared pool, 8 goroutines)

| Benchmark | ops/sec | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| Shared pool | 11.3M | 105 | 4 | 0 |

¹ 4 B/op, 0 allocs/op — `sync.WaitGroup` stack spill in benchmark scaffolding, not a heap allocation.

### GC Isolation (GODEBUG=gctrace=1)

Sustained 10+ second runs under `GODEBUG=gctrace=1`. Every allocation path shows **`0->0->0 MB`** live heap with zero automatic GC triggers.

| Benchmark | Duration | GC Cycles | Live Heap | Auto GC |
|---|---|---|---|---|
| HotPath | 10s | 7 forced | 0→0→0 MB | 0 |
| GrowPath | 5s | 4 forced | 0→0→0 MB | 0 |
| LargeAllocation | 5s | 4 forced | 0→0→0 MB | 0 |

**What `0->0->0 MB` means** (gctrace format: `live_before→live_marked→live_after`):

- **Before GC:** zero bytes considered live by the runtime
- **After mark:** zero bytes survived marking (nothing to trace)
- **After sweep:** zero bytes remain

All GC cycles are `(forced)` — `runtime.GC()` from benchmark scaffolding (`BenchmarkGoHeapUsed`), not pressure-driven. No automatic GC fired because the runtime never detected heap growth.

### Platform Notes

Since all memory is `mmap`/`madvise`-backed, RSS behavior after `Reset()` varies by platform:

| Platform | `MADV_DONTNEED` behavior | RSS after Reset |
|---|---|---|
| **Linux** | `MADV_DONTNEED` releases pages immediately | RSS drops |
| **macOS (darwin)** | `MADV_FREE` lazily reclaims pages | RSS may linger until memory pressure |
| **Windows** | `VirtualFree` varies by call type | — |

On macOS, `top`/`htop` may show higher resident memory after `Reset()` due to lazy page reclamation. This is **cosmetic** — the OS reclaims pages under pressure. Go runtime metrics (`MemStats`) will always report zero heap growth.

## What This Is NOT

- **Not GC-safe** — memory is not zeroed on alloc/reset; caller manages contents
- **Not thread-safe for `Arena`** — single-producer bump allocator, concurrent use causes corruption
- **Not a substitute for `sync.Pool`** — designed for explicit lifecycle control, not automatic GC integration
- **Not a general-purpose allocator** — tuned for slab workloads; large allocations bypass slabs
