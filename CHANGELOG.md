# Changelog

## [v1.0.3] — 2026-06-12

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
