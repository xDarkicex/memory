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
- `freelist_helpers.go` — typed allocation helpers (`FreeListAlloc[T]`, `FreeListDealloc[T]`)
- `hyaline.go` — Hyaline batch management for `ShardedFreeList`
- `sharded_freelist.go` — `ShardedFreeList` (sharded wrapper around FreeList)
- `shard.go` — per-shard data structures: `shardCache`, `freshCache`, `ringBuf`
- `shard_hash.go` / `shard_procpin.go` — `getShard()` implementations
- `stats.go` — GC stats, memory profiles, `ZeroMemory`, `Hint` declaration
- `watchdog.go` — Go heap pressure monitor (not related to off-heap mmap memory)
- `memory_linux.go` / `memory_darwin.go` — platform-specific mmap + madvise

### Slot metadata protocol

#### FreeList

Each slot stores intrusive metadata at fixed offsets within the slot:
- **Offset 0**: next pointer (uint64, Treiber stack link)
- **Offset 12**: `metaOffset` — typed user data begins here (`FreeListAlloc[T]` helpers)
- **Offset 24**: packed uint32 — structIdx in bits 0-23 (homeShard always 0)

Metadata at offset 24 lives inside the user data region (past `metaOffset=12`). Callers that preserve it get O(1) lock-free `Deallocate`; callers that overwrite it trigger the O(log N) binary search fallback over `slabBase`.

Minimum SlotSize: **32** (enforced in `NewFreeList`).

#### ShardedFreeList (Hyaline)

Wraps a FreeList with per-shard caches and Hyaline batch metadata:
- **Offset 0**: next pointer (uint64, slot chain link)
- **Offset 8**: batch_head (uint64, pointer to batch head node)
- **Offset 16**: batch_next (uint64, next node in same batch)
- **Offset 24**: refs (int64, batch reference count; batch head only)
- **Offset 32**: first_node (uint64, batch.first stored at flush; batch head only)
- **Offset 40**: packed uint32 — bits 0-23 = slab struct index, bits 24-31 = home shard index

Total Hyaline overhead: 44 bytes (padded to 48 for alignment). Minimum usable SlotSize: **48**.

`pushFree` writes structIdx (+ homeShard for ShardedFreeList) at the metadata offset; `Allocate` and `Deallocate` read it to resolve the owning slab without locks or binary search. `Deallocate` falls back to O(log N) binary search over `slabBase` (sorted by mmap base address) when metadata is corrupted.
