# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```
go test ./...              # Run all tests
go test -race ./...        # Run all tests with race detector
go vet ./...               # Static analysis
go test -bench=. -benchmem ./...  # Run benchmarks with memory stats
go test -run TestFoo ./... # Run a single test
go build -tags procpin -ldflags=-checklinkname=0 ./...  # Build with P-bound sharding
```

## Architecture

This is an off-heap memory allocator library for Go. Allocations live in mmap'd memory invisible to the Go GC. The sole external dependency is `golang.org/x/sys` for the `unix` package.

### Allocator hierarchy

Four allocator types, each for different use cases:

| Type | Allocation | Free model | Concurrency |
|------|-----------|------------|-------------|
| `Pool` | Variable-size (CAS slab allocator) | Bulk `Reset()` | Lock-free multi-producer |
| `Arena` | Variable-size (CAS bump pointer) | `Reset()` (rewind) or `Free()` (destroy) | Single-producer recommended |
| `FreeList` | Fixed-size (Treiber stack) | Per-object `Deallocate()` | Lock-free |
| `ShardedFreeList` | Fixed-size (wraps FreeList + per-shard caches) | Per-object `Deallocate()` | Lock-free, sharded by goroutine |

### Key design invariants

- **Zero heap allocations on hot paths** — all backing arrays (slabBuf, slabStructs, largeBuf, slotGen) are pre-allocated at construction and never resized.
- **Generation counters** — `Reset()` increments a generation before unmapping slabs; allocators check the generation before and after CAS to avoid returning pointers into unmapped memory. Best-effort; the real guarantee is caller-enforced quiescence.
- **ABA protection** — FreeList uses tagged pointers (16-bit generation in upper bits of uint64 head). Requires ≤48-bit virtual addresses; LA57 kernels are detected and rejected at `NewFreeList`.
- **8-byte alignment** — all allocations are aligned for SIMD/ARM.

### Platform split

Platform-specific code uses Go build tags:

- `memory_linux.go` / `memory_darwin.go` — `Pool.mmapSlab`, `FreeList.mmapSlab`, and `Hint()` with platform-appropriate `madvise` flags. Linux supports `MAP_HUGETLB` + `MADV_HUGEPAGE`; Darwin ignores huge pages.
- `memory_linux_autodetect.go` / `memory_darwin_autodetect.go` — `init()` functions that set `HugepageSize`. Linux reads `/proc/meminfo`; Darwin sets it to 0 (no huge page support).
- `shard_hash.go` (default) / `shard_procpin.go` (opt-in via `-tags procpin`) — `getShard()` function for ShardedFreeList. Default uses stack-address hash; procpin uses `runtime.procPin` for P-bound affinity.

### file layout rationale

- `allocator.go` — `AllocatorConfig`, `DefaultConfig`, error sentinels, `PageSize`, `HugepageSize`
- `pool.go` — `Pool` type (concurrent slab allocator)
- `arena.go` — `Arena` type (bump-pointer allocator)
- `freelist.go` — `FreeList` type (fixed-size lock-free allocator) + tagged pointer helpers
- `sharded_freelist.go` — `ShardedFreeList` (sharded wrapper around FreeList)
- `shard.go` — per-shard data structures: `shardCache`, `freshCache`, `ringBuf`
- `shard_hash.go` / `shard_procpin.go` — `getShard()` implementations
- `stats.go` — GC stats, memory profiles, `ZeroMemory`, `Hint` declaration
- `watchdog.go` — Go heap pressure monitor (not related to off-heap mmap memory)
- `memory_linux.go` / `memory_darwin.go` — platform-specific mmap + madvise

### Slot metadata protocol (FreeList / ShardedFreeList)

Each free slot stores:
- **Offset 0**: next pointer (for intrusive Treiber stack / Hyaline node chain)
- **Offset 8**: batch_link (Hyaline: link to batch head for reference counting)
- **Offset 16**: refs (on batch head) / batch_next (on other nodes) — Hyaline reclamation
- **Offset 24**: packed uint32 — `bits[0:24]` = slab struct index, `bits[24:32]` = home shard index (ShardedFreeList only)

Total overhead: 28 bytes (padded to 32 for alignment). Minimum SlotSize: 32.

`pushFree` writes the metadata; `Allocate` reads structIdx from offset 24 to resolve the owning slab without locks or binary search. `Deallocate` uses O(log N) binary search over `slabBase` (sorted by mmap base address) as a fallback when offset 24 metadata is corrupted.
