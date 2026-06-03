# Changelog

## [Unreleased]

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
