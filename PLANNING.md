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
    │  ┌─────────┐  ┌──────────────────┐          │
    │  │ LIFO    │  │ Remote Return Q  │          │
    │  │ Array   │  │ (MPSC ring buf)  │          │
    │  └────┬────┘  └────────┬─────────┘          │
    │       │                │                     │
    │       │   Underflow    │                     │
    │       ▼                ▼                     │
    │  ┌──────────────────────────────────┐       │
    │  │  Shard‑Local Hazard Registry     │       │
    │  │  (K=2 slots per shard)           │       │
    │  └──────────────┬───────────────────┘       │
    └─────────────────┼───────────────────────────┘
                      │
              ┌───────▼────────┐
              │  Global Pool    │  Existing FreeList
              │  (batch‑pop)    │  + batch operations
              └───────┬────────┘
                      │
              ┌───────▼────────┐
              │  Slab Allocator │  mmap'd off‑heap
              │  + Retirement   │  memory
              └────────────────┘
```

## Slot Layout

```
Offset  Size   Field
[0:8]   8B     Next pointer (intrusive freelist link when free)
[8:12]  4B     Packed metadata:
                • structIdx  (20 bits — up to 1M slabs)
                • homeShard  (8 bits — up to 256 shards)
                • state      (4 bits — free/allocating/allocated/retired)
[12:16] 4B     User data start (minimum SlotSize = 16)
[16:...]       User data (for SlotSize > 16)
```

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

- [ ] **2.1: Shard index selection**
  - Implement `runtime_procPin` binding (build tag: `procpin`)
  - Implement hash‑based fallback (build tag: `!procpin`)
  - Unit tests: shard distribution uniformity (chi‑squared)
  - Benchmark: shard index computation overhead

- [ ] **2.2: Per‑shard LIFO cache**
  - Fixed‑size array per shard (capacity = 64 slots)
  - Pop: decrement index, return slot (no atomics)
  - Push: increment index, store slot (no atomics)
  - Underflow: call global FreeList.BatchPop()
  - Unit tests: LIFO correctness, underflow behavior
  - Benchmark: alloc+free pair via per‑shard cache (expect <10ns)

- [ ] **2.3: Slot metadata packing**
  - Pack structIdx (20b) + homeShard (8b) + state (4b) into uint32 at offset 8
  - Helper functions: `packMeta()`, `unpackStructIdx()`, `unpackHomeShard()`, `unpackState()`
  - Update pushFree to write metadata
  - Update Allocate to read metadata
  - Unit tests: round‑trip pack/unpack, bitfield boundaries

- [ ] **2.4: Remote return mechanism**
  - Per‑shard MPSC ring buffer (lock‑free for producers/consumer)
  - On Deallocate: if homeShard != currentShard, push to home shard's remote queue
  - On LIFO underflow: drain remote queue before hitting global pool
  - Fallback for queue full: push to global FreeList directly
  - Unit tests: cross‑shard alloc/free cycles
  - Benchmark: cross‑shard free throughput

- [ ] **2.5: Integrate sharded path into public API**
  - `NewShardedFreeList(cfg)` — creates N shards + global pool
  - `Allocate()` — shard select → LIFO pop → batch refill → fallback
  - `Deallocate(slot)` — check home shard → local LIFO or remote queue
  - `Stats()` — aggregated across shards
  - `Reset()` / `Free()` — clear shards + global pool
  - Unit tests: full lifecycle, exhaustion, concurrent safety

### Phase 3 — Hazard Pointers

- [ ] **3.1: Hazard pointer registry (per shard)**
  - K=2 hazard slots per shard
  - Publication: `atomic.StorePointer(&hazard[i], ptr)`
  - Validation: `atomic.LoadPointer(&head)` after publication
  - Clear: `atomic.StorePointer(&hazard[i], nil)`
  - Unit tests: publication/validation/clear lifecycle
  - Benchmark: publication overhead on ARM64 vs x86_64

- [ ] **3.2: Retirement list (per shard)**
  - Per‑shard private retirement list (slice of `unsafe.Pointer`)
  - `Retire(ptr)`: append to list, check threshold
  - Threshold: R = H + 32, where H = numShards × 2
  - Unit tests: threshold triggering, list overflow behavior

- [ ] **3.3: Hazard pointer scan**
  - Snapshot: copy all active hazard pointers from all shards into flat `[]uintptr`
  - For each retired node: linear scan against snapshot
  - Safe nodes → push to global freelist
  - Unsafe nodes → remain in retirement list
  - Unit tests: reclaim safe vs retain unsafe
  - Benchmark: scan time at N=[16,32,64,128] shards

- [ ] **3.4: Integrate scan with allocation backpressure**
  - When global pool `BatchPop` returns nil AND retirement list exceeds threshold:
    → allocate from goroutine: trigger scan → reclaim → retry BatchPop
  - Ensures bounded memory without background goroutines
  - Unit tests: backpressure path, no deadlocks

### Phase 4 — Performance Validation & Documentation

- [ ] **4.1: Full‑stack benchmark suite**
  - `BenchmarkShardedHotPath` — single‑goroutine alloc+free
  - `BenchmarkShardedConcurrent` — 8/16/32/64 goroutines, alloc+free loop
  - `BenchmarkShardedCrossShard` — forced cross‑shard frees
  - `BenchmarkShardedScan` — amortized scan overhead at steady state
  - Log all results to `BENCHMARK.md` with before/after comparisons

- [ ] **4.2: Race‑detector stress test**
  - 100× `go test -race -count=1` on sharded tests
  - Allocate/Deallocate storms concurrent with Reset
  - Cross‑shard free storms

- [ ] **4.3: GC isolation verification**
  - `GODEBUG=gctrace=1` on sustained benchmark runs
  - Verify `0→0→0 MB` live heap across all paths
  - Verify zero automatic GC triggers

- [ ] **4.4: Documentation**
  - Update `README.md`: sharded allocator section, build tag docs, benchmark results
  - Update `BENCHMARK.md`: final numbers with tables
  - API godoc: ShardedFreeList, hazard pointer guarantees, slot layout
  - `CONTRIBUTING.md`: build tag conventions, benchmark harness docs

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
    2.4 (remote return) ◄── 1.4 result
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

## Gating Decisions

| Gate | Task | Condition | Outcome |
|------|------|-----------|---------|
| G1 | 1.2 | ops/sec flat across GOMAXPROCS | Skip sharding; bottleneck is memory BW |
| G2 | 1.3 | batch‑pop < 2× faster than N× popFree | Use individual pops (simpler) |
| G3 | 1.4 | cross‑shard frees < 5% | mutex+slice remote queue (simpler) |
| G4 | 3.3 | scan < 20µs at 64 shards | Keep linear scan; no SIMD needed |
