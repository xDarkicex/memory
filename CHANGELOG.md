# Changelog

## [v1.1.0] — 2026-07-06

### Added

- **`HashMap`** — zero-allocation, wait-free concurrent map backed by mmap'd
  off-heap memory. CAS-based publishing with cooperative migration (Dr. Cliff
  Click's wait-free hash map design). SWAR fingerprint filtering for 2-cycle
  multi-slot evaluation. 128-byte cache-aligned buckets optimized for
  AVX-512 / Apple Silicon. Generic `TypedMap[V]` wrapper.
- **`HashMap` RAG benchmarks** — index build (10K vectors), concurrent lookup,
  and mixed workload vs `sync.Map` and `sync.RWMutex`. 10,000× less heap than
  `sync.Map`, 1.8–2.0× faster on concurrent workloads.
- **Typed allocation helpers for ShardedFreeList** — `ShardedFreeListAlloc[T]`,
  `ShardedFreeListDealloc[T]`, `ShardedFreeListSlotFor[T]`, and `Must` variants.

### Changed

- **Breaking: alignment parameter added to all allocator constructors.** All
  four constructors now accept an explicit alignment parameter for SIMD/AVX-512
  memory alignment:

  | Constructor | Old signature | New signature |
  |---|---|---|
  | `NewArena` | `(size uint64)` | `(size uint64, align uint64)` |
  | `NewPool` | `(cfg AllocatorConfig)` | `(cfg AllocatorConfig, align uint64)` |
  | `NewFreeList` | `(cfg FreeListConfig)` | `(cfg FreeListConfig, align uint64)` |
  | `NewShardedFreeList` | `(cfg FreeListConfig, numShards int)` | `(cfg FreeListConfig, align uint64, numShards int)` |

  Passing `0` selects the default 64-byte alignment (AVX-512 / VMOVAPS safe).
  Non-power-of-2 values return an error. Previously, alignment was hardcoded
  to 8 bytes.

- **HashMap values must be off-heap pointers.** The GC never scans the mmap'd
  bucket array — Go heap pointers stored in the map are invisible to the
  collector and will be freed. Use Arena, FreeList, Pool, or ShardedFreeList
  allocations for all values. The race detector's checkptr instrumentation
  catches Go heap pointers at test time.

### Fixed

- `mapState.base` changed from `uintptr` to `unsafe.Pointer` with
  single-expression pointer arithmetic to satisfy `checkptr` under `-race`.
- HashMap tests and benchmarks use Arena-allocated off-heap values, consistent
  with every other allocator in the package. Full `-race` + `checkptr` pass.

### Added

- **`MmapFile`, `MmapFileReadOnly`, `Munmap`** — cross-platform public API for
  file-backed shared memory mappings. `MmapFileReadOnly(fd, offset, size)` maps a
  file descriptor as a read-only shared mapping. `MmapFile(fd, offset, size, writable)`
  supports both read-only (`writable=false`, equivalent to `MmapFileReadOnly`) and
  read-write (`writable=true`) shared mappings where modifications propagate to the
  underlying file via the kernel page cache. `Munmap(data)` unmaps a previously
  mapped region. Platform support:
  - **Unix** (`mmap_unix.go`): `PROT_READ` / `PROT_READ|PROT_WRITE` + `MAP_SHARED`;
    `munmap(2)` for unmap.
  - **Windows** (`mmap_windows.go`): `PAGE_READONLY` / `PAGE_READWRITE` +
    `CreateFileMapping` + `MapViewOfFile`; `UnmapViewOfFile` for unmap.
- **`mmap_test.go`** — 19 tests covering read-only, writable, shared mapping
  semantics, offset/partial-size mappings, multiple maps on the same fd, edge cases
  (invalid fd, negative offset, empty file, zero-size map, map-beyond-file-end),
  Munmap safety (nil, empty, double-unmap), and a 100-iteration map/unmap round-trip
  stress test. All tests pass on macOS (arm64) and are cross-platform.

## [v1.0.2] — 2026-06-08

### Fixed

- **Goroutine leak in `ShardedFreeList`** — `Free()` and `Reset()` called
  `cancel()` to signal the background PID controller goroutine but returned
  without waiting for it to exit, triggering goleak detections. Added a
  `pidDone` channel that the goroutine closes on exit; `Free()` and `Reset()`
  now drain it before proceeding.

- **`TestStressHammer` timeout under `-race`** — `growSlab()` held
  `slabMu.Lock()` while pushing every slot in a new slab to the Treiber stack
  (up to 32K CAS operations per 4MB slab). Under the race detector, each
  instrumented CAS becomes pathologically slow, blocking all `Deallocate`
  slow-path readers and starving the pool. Moved `slabMu.Unlock()` before the
  push loop. No correctness impact: `Reset()` is already documented as not
  concurrent-safe, and the double-check + lock still prevents duplicate slab
  publishing.

### Added

- **Goleak tests** — `TestShardedFreeListPIDControllerFree` and
  `TestShardedFreeListPIDControllerReset` verify no goroutine leaks after
  `Free()` and `Reset()`.
- `go.uber.org/goleak` test dependency.

### Fixed

- **checkptr false positive** in Treiber stack speculative reads (`shard.go`,
  `freelist.go`, `hyaline.go`, `sharded_freelist.go`) — `//go:nocheckptr`
  directives added for off-heap pointer arithmetic. The `pop()` speculative read
  is validated by subsequent CAS; the stale value is never used, but checkptr
  panics before the CAS can validate.

- **`TestPoolSizeNeverExceeded`** property test — `allocSize % (PoolSize/2)`
  can produce 0, causing `ErrInvalidSize` that the test's error handler didn't
  expect. Fixed with a zero-guard before `Allocate()`.

- **`BenchmarkFreeListVsPool_64B/Pool`** — Pool has no per-allocation free (only
  bulk `Reset()`); the benchmark exhausted the 64MB pool. Fixed with periodic
  Reset at ~50% capacity.

### Added

- Test coverage: 86.8% → 90.8% on the main package. Zero 0%-coverage functions
  remain.
- 17,138 stress-test runs at 16 parallel, zero failures.
- Five slabby-vs-memory competition benchmarks in `competition_bench_test.go`
  covering BFS buffer pools, edge bulk allocation, page table bulk allocation,
  and concurrent AddEdge workloads.
- `CHANGELOG.md` (this file).
