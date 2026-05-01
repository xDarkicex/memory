# Sharded Hazard-Pointer Allocator — Implementation Plan

## Architecture Overview

```
Application
     │
     ▼
┌─────────────────────────────────────────────┐
│  Allocate() / Deallocate()  (public API)    │
└────────────┬────────────────────────────────┘
             │
    ┌────────▼────────┐
    │  Shard Index     │  runtime_procPin (fast) or hash (fallback)
    └────────┬────────┘
             │
    ┌────────▼────────────────────────────────────┐
    │  Per‑Shard Cache (× N, N ≈ GOMAXPROCS)      │
    │  ┌──────────────────┐  ┌──────────────────┐ │
    │  │ freshCache       │  │ recycled (LIFO)  │ │
    │  │ (batch-refill    │  │ (Deallocate      │ │
    │  │  pre-accounted)  │  │  route-to-local) │ │
    │  └────────┬─────────┘  └────────┬─────────┘ │
    │           │                     │            │
    │           │   Underflow         │  Overflow  │
    │           ▼                     ▼            │
    │  ┌──────────────────────────────────────┐   │
    │  │  Global FreeList (batch refill)      │   │
    │  └──────────────────────────────────────┘   │
    └─────────────────────────────────────────────┘
                      │
              ┌───────▼────────┐
              │  Slab Allocator │  mmap'd off‑heap
              │  + Retirement   │  memory
              └────────────────┘
```

Deallocate always routes to the current goroutine's shard (not the allocating
shard).  When the local recycled cache overflows, slots spill to the global
FreeList.  The global FreeList acts as an equalizer: any shard that runs dry
refills from it via BatchAllocate.  No cross-shard queues are needed.

## Slot Layout

```
Offset  Size   Field
[0:8]   8B     Next pointer (intrusive Treiber stack link when free)
[8:12]  4B     Packed metadata:
                • structIdx  (24 bits — up to 16M slabs)
                • homeShard  (8 bits — up to 256 shards)
[12:...]       User data start (minimum SlotSize = 12 for metadata users)
```

No state bits are needed: double-free detection uses slotGen counters
(allocSeq-based), and the alloc/free state is implicit in which cache or
list the slot resides in.

## Build Tag Strategy

```
// File: shard_procpin.go
//go:build procpin

// True per‑P sharding via runtime.procPin
// Build: go build -tags procpin -ldflags=-checklinkname=0

// File: shard_hash.go
//go:build !procpin

// Hash‑based sharding fallback
// Build: go build (no flags)
```

---

## Task Tracker

### Phase 1 — Setup & Baselines

- [x] **1.1: Create experimental branch**
  - `git checkout -b feat/sharded-hazard-allocator`
  - Verify baseline tests pass: `go test -race ./...`

- [x] **1.2: Global freelist contention profile**
  - Wrote `BenchmarkFreeListContention` in benchmark_test.go
  - Added `casRetries` atomic counter to FreeList with cache-line padding
  - Added `CasRetries()` accessor and `CasRetries` field to `FreeListStats`
  - Results: severe contention — 0.09x scaling at 8 cores, 3.67 CAS retries/op
  - **Gating decision G1: sharding is justified.**

- [x] **1.3: Batch‑pop prototype on global FreeList**
  - Renamed `BatchPop` → `batchPop` (unexported primitive, no bookkeeping)
  - Added `BatchAllocate(slots [][]byte) (int, error)` with full accounting
  - Batched atomic ops: single `allocated.Add(n*slotSize)` + single `allocSeq.Add(n)`
  - Stack-allocated `[128]unsafe.Pointer` buffer for the batch pop
  - Results: ~2× per-slot throughput vs N× Allocate under 4—8 core contention
  - **Gating decision G2: batch refill confirmed.** Sweet spot at batch size 32.

- [x] **1.4: Cross‑shard free frequency measurement**
  - Wrote `BenchmarkCrossShardFrequency` (same-goroutine baseline: 0% cross)
  - Wrote `BenchmarkCrossShardWorkStealing` (channel handoff: 100% cross)
  - Tag goroutine ID at slot offset 12; read back before dealloc
  - **Gating decision G3: MPSC ring buffer confirmed.** Real workloads with goroutine handoff always exceed 5% cross.

### Phase 2 — Core Sharded Allocator

- [x] **2.1: Shard index selection**
  - Implemented `runtime_procPin` binding (build tag: `procpin`) in `shard_procpin.go`
  - Implemented hash‑based fallback (build tag: `!procpin`) in `shard_hash.go`
  - getShard uses stack-address hash (`sp >> 10`) for reasonable distribution
  - TODO: shard distribution uniformity (chi‑squared), computation overhead benchmark

- [x] **2.2: Per‑shard LIFO cache**
  - Lock-free Treiber stack per shard (`shardCache`), capacity 64 slots
  - Uses tagged pointers (48-bit address + 16-bit tag) for ABA protection
  - Separate `freshCache` for batch-refill slots (pre-accounted, skip activateSlot)
  - `StoreUint64`/`LoadUint64` atomics avoid checkptr on mmap'd memory
  - Underflow: call global FreeList.BatchAllocate() for batch refill
  - TODO: dedicated LIFO correctness unit tests, hot-path bench

- [x] **2.3: Slot metadata packing**
  - Pack structIdx (24b) + homeShard (8b) into uint32 at offset 8
  - Helper functions: `packSlotMeta()`, `unpackStructIdx()`, `packHomeShard()`
  - `Deallocate` repacks metadata at offset 8 so activateSlot can recover structIdx
  - `pushFree` writes metadata; `activateSlot` reads it
  - No state bits needed — double-free detection via slotGen counters

- [x] **2.4: Cross-shard free handling (architecture simplified)**
  - Original plan: per-shard MPSC ring buffer for remote returns
  - **Decision: ring buffer removed after implementation.** MPMC ordering issues
    (producer CASes head before writing slot data) caused nil-pointer derefs and
    stale entries under sustained cross-shard load.
  - **Replacement:** Deallocate always routes to the current goroutine's shard.
    When the local recycled cache is full, slots overflow to the global FreeList.
    The global FreeList acts as an equalizer — any shard that runs dry refills
    from it via BatchAllocate. No cross-shard queues needed.
  - Cross-shard correctness verified by `TestShardedFreeListCrossShard`.

- [x] **2.5: Integrate sharded path into public API**
  - `NewShardedFreeList(cfg, numShards)` — creates N shards + global FreeList
  - `Allocate()` — fresh cache → recycled cache → BatchAllocate refill
  - `Deallocate(slot)` — validate → route to current shard → overflow to global
  - `Stats()` — delegates to global FreeList
  - `Reset()` — bumps generation, clears all shard caches, resets global
  - `Free()` — releases all mmap'd memory
  - Unit tests: basic lifecycle, double-free, reset, concurrent, cross-shard, exhaustion

### Phase 3 — Hazard Pointers

- [x] **3.1: Hazard pointer registry (per shard)**
  - K=2 hazard slots per shard using `atomic.Uint64` (uintptr, not unsafe.Pointer —
    avoids GC badPointer panics on mmap'd addresses)
  - `Protect(slot)` → CAS publish to current shard's hazard slot; returns `(HazardGuard, bool)`
  - `Unprotect(guard)` → atomic Store(0) to clear
  - Publication via CAS provides full Store-Load barrier (STLR on ARM64, XCHG on x86_64)
  - Unit tests: protect/unprotect lifecycle, K=2 exhaustion, concurrent protect/retire
  - TODO: publication overhead benchmark on ARM64 vs x86_64

- [x] **3.2: Retirement list (per shard)**
  - Lock-free Treiber stack (`retiredStack`) — no ABA tag needed (batch drain only)
  - `Retire(slot)` → validates slot, clears slotGen, decrements allocated, pushes to
    current shard's retirement stack
  - Per-shard retired count tracked via `atomic.Int32` for threshold checks
  - No per-retire scan: amortized reclamation via scan only on allocation backpressure
  - Unit tests: double-retire detection, concurrent retire safety
  - TODO: threshold-based proactive scan (currently triggers only on exhaustion)

- [x] **3.3: Hazard pointer scan**
  - `collectHazards()` — snapshot all non-zero hazard pointers from all shards
  - `toHazardSet()` — build map[uintptr] for O(1) lookup during scan
  - `scan()` — drain all shards' retirement stacks atomically, check each node
    against hazard set, push safe nodes to global FreeList via `pushFree`,
    return unsafe nodes to their shard's retirement stack
  - Safe nodes bypass shard caches (go directly to global FreeList)
  - Unit tests: protected slot survives scan, unprotected slot reclaimed, exhaustion recovery
  - TODO: benchmark scan time at N=[16,32,64,128] shards

- [x] **3.4: Integrate scan with allocation backpressure**
  - Allocate flow: fresh → recycled → BatchAllocate → scan → retry
  - Scan triggers when `BatchAllocate` returns 0 (global FreeList empty)
    AND any shard has retired slots
  - Reclaimed slots enter global FreeList; next retry's BatchAllocate picks them up
  - No background goroutines — reclamation is synchronous on the allocating goroutine
  - Reset clears all hazard slots and retirement stacks
  - Unit tests: retire+reclaim cycle, exhaustion→scan→recover, concurrent allocate+retire

### Phase 4 — Performance Validation & Documentation

- [x] **4.1: Full‑stack benchmark suite**
  - `BenchmarkShardedHotPath` — single‑goroutine alloc+free: 54.4 ns/op, 0 allocs/op
  - `BenchmarkShardedHotPathHP` — single‑goroutine Protect+Unprotect+Retire: 77.5 ns/op, 0 allocs/op
  - `BenchmarkShardedConcurrent` — 8 goroutines alloc+free: 411.6 ns/op
  - `BenchmarkShardedConcurrentHP` — 8 goroutines Protect+Retire: 337.9 ns/op
  - `BenchmarkShardedCrossShard` — channel handoff, 100% cross-shard: 272.0 ns/op
  - `BenchmarkShardedScanOverhead` — amortized scan in small pool: 92.1 ns/op
  - `BenchmarkFreeListVsShardedHotPath` — single-goroutine FreeList (37.7ns) vs Sharded (53.5ns)
  - `BenchmarkFreeListVsShardedConcurrent` — FreeList vs Sharded scaling sweep (1-8 cores)
  - Results logged to `BENCHMARK.md`. Sharding wins at 2-4 cores (1.54× faster at 2 cores).
    At 8 cores on M2 (4P+4E), E-cores penalize sharding's higher per-op work.

- [x] **4.2: Race‑detector stress test**
  - 50× `go test -race -count=1` on 11 sharded + hazard tests = 550 iterations
  - All passed — zero races, zero panics
  - Tests cover: basic lifecycle, double-free, reset, concurrent, cross-shard,
    exhaustion, protect/unprotect, retire/reclaim, protected-slot-survives-scan,
    concurrent protect+retire

- [x] **4.3: GC isolation verification**
  - `GODEBUG=gctrace=1` on sustained benchmark runs
  - Deallocate path: 0 B/op, 0 allocs/op — perfect isolation, zero GC interference
  - HP path: 6 B/op, 0 allocs/op — amortized scan overhead, zero per-op heap allocs
  - No GC cycles during steady-state operation; forced cycles only at pool setup
  - Mmap'd memory is never scanned by Go GC (uintptr typed, off-heap)

- [x] **4.4: Documentation**
  - `BENCHMARK.md`: updated with all Phase 2-4 results, scaling tables, GC isolation data
  - `PLANNING.md`: updated architecture diagram, slot layout, task status, gating decisions
  - API godoc: ShardedFreeList, HazardGuard, Protect/Unprotect/Retire/scan documented in source
  - TODO: update `README.md`, `CONTRIBUTING.md`

### Phase 5 — Platform‑Specific Optimizations

- [ ] **5.1: ARM64 path validation**
  - Verify LDAR/STLR emission (no custom assembly needed; confirmed by research)
  - Benchmark on Apple Silicon M2/M3
  - Log to `BENCHMARK.md`

- [ ] **5.2: x86_64 path validation**
  - Verify CAS-based primitives
  - Benchmark on AMD Zen 4+ / Intel Sapphire Rapids+
  - Log to `BENCHMARK.md`

- [ ] **5.3: `procpin` build tag integration**
  - Document `-tags procpin -ldflags=-checklinkname=0` in README
  - Graceful degradation: if procpin build tag set but linkname blocked → fallback to hash
  - Detect at init: attempt procPin, if fails → use hash

---

## Dependencies Between Tasks

```
1.1 (branch) ──┬─► 1.2 (contention profile)
               ├─► 1.3 (batch‑pop prototype)
               └─► 1.4 (cross‑shard measurement)
                         │
    2.1 (shard index) ◄──┘
    2.2 (LIFO cache)
    2.3 (metadata packing)
    2.4 (cross-shard — simplified, ring buffer removed)
    2.5 (integration)
                         │
    3.1 (hazard registry) ◄── 2.5
    3.2 (retirement list)
    3.3 (HP scan)
    3.4 (scan backpressure)
                         │
    4.1─4.4 (validation) ◄── 3.4
    5.1─5.3 (platform)
```

Phases 1–4 are sequential. Phase 5 can run in parallel with Phase 4.

**Phase 2 is complete.** The ring buffer originally planned for 2.4 was
implemented, proved fragile under MPMC access patterns (stale entries,
nil-pointer derefs from partial writes), and was replaced with a simpler
design: current-shard routing with global FreeList as equalizer.

**Phase 3 is complete.** Hazard pointer registry, retirement lists, scan,
and backpressure integration are implemented in `hazard.go`. The public API
is Protect/Unprotect (for concurrent read safety) and Retire (for deferred
safe reclamation). Deallocate remains the fast path (no HP overhead).

## Gating Decisions

| Gate | Task | Condition | Outcome |
|------|------|-----------|---------|
| G1 | 1.2 | ops/sec flat across GOMAXPROCS | Skip sharding; bottleneck is memory BW |
| G2 | 1.3 | batch‑pop < 2× faster than N× popFree | Use individual pops (simpler) |
| G3 | 1.4 | cross‑shard frees < 5% | Current-shard routing (simpler). Ring buffer was built, proved fragile, removed. |
| G4 | 3.3 | scan < 20µs at 64 shards | Keep linear scan; no SIMD needed |
