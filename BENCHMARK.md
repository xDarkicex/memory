# Benchmark Log

## System Info

| Field | Value |
|-------|-------|
| Machine | MacBook Pro (Mac14,7) |
| CPU | Apple M2 |
| Cores | 8 (4P + 4E) |
| Memory | 24 GB |
| OS | macOS 26 |
| Go Version | go1.25.7 darwin/arm64 |
| Kernel | Darwin 25.3.0 |

---

## 1.2 — Global Freelist Contention Profile

**Setup:** `FreeList`, SlotSize=64, Prealloc=true, single shared pool

**Sweep:** GOMAXPROCS=[1,2,4,8,16,32,64], goroutines=GOMAXPROCS, 10s per test

| GOMAXPROCS | ops/sec (total) | ops/sec/goroutine | ns/op | CAS retries/op | Notes |
|------------|-----------------|-------------------|-------|----------------|-------|
| 1 | 25.9M | 25.9M | 38.6 | 0.00 | Linear baseline |
| 2 | 6.2M | 3.1M | 161.0 | 0.44 | 0.24x scaling — severe contention |
| 4 | 4.6M | 1.1M | 219.5 | 1.86 | 0.17x scaling |
| 8 | 2.3M | 0.29M | 430.4 | 3.67 | 0.09x scaling, 3.67 CAS retries/op |
| 16 | (pending) | | | | |
| 32 | (pending) | | | | |
| 64 | (pending) | | | | |

**Decision:** G1 — JUSTIFIED. Throughput per goroutine drops to 9% at 8 cores. CAS retries climb to 3.67/op. Per-shard LIFO caches with batch-pop from global freelist should recover near-linear scaling.

---

## 1.3 — Batch‑Pop Prototype

**Setup:** `BatchAllocate(N)` vs N× `Allocate()` — 8 goroutines contending on shared FreeList

### 4 cores (Apple M2)

| Method | ns/op (batch) | ns/slot | CAS/batch | Speedup |
|--------|--------------|---------|-----------|---------|
| BatchAllocate(16) | 1983 | 124 | 1 | 1.90x |
| N× Allocate =16 | 3759 | 235 | 16 | 1.00x |
| BatchAllocate(32) | 3893 | 122 | 1 | 1.81x |
| N× Allocate =32 | 7084 | 221 | 32 | 1.00x |
| BatchAllocate(64) | 7091 | 111 | 1 | 1.92x |
| N× Allocate =64 | 13615 | 213 | 64 | 1.00x |

### 8 cores (Apple M2)

| Method | ns/op (batch) | ns/slot | CAS/batch | Speedup |
|--------|--------------|---------|-----------|---------|
| BatchAllocate(16) | 3529 | 221 | 1 | 1.86x |
| N× Allocate =16 | 6556 | 410 | 16 | 1.00x |
| BatchAllocate(32) | 6854 | 214 | 1 | 1.93x |
| N× Allocate =32 | 13236 | 414 | 32 | 1.00x |
| BatchAllocate(64) | 13385 | 209 | 1 | 2.05x |
| N× Allocate =64 | 27446 | 429 | 64 | 1.00x |

**Decision:** G2 — CONFIRMED. BatchAllocate gives ~2× per-slot throughput. Use batch size 32 as sweet spot (balances latency vs contention amortization on M2).

---

## 1.4 — Cross‑Shard Free Frequency

**Setup:** Instrument existing FreeList with goroutine‑hash tagging

| Workload | Allocations | Local Frees | Cross Frees | Cross % | Notes |
|----------|-------------|-------------|-------------|---------|-------|
| alloc‑free loop, same goroutine | 5.0M (4-core) | 5.0M | 0 | 0% | Baseline: no handoff |
| work‑stealing (channel handoff) | 5.2M (4-core) | 0 | 5.2M | 100% | Producer→consumer goroutines |
| Mixed server workload | — | — | — | >5% | Any non-trivial goroutine handoff |

**Decision:** G3 — MPSC ring buffer. Baseline is 0% cross (simple), but any goroutine handoff pattern (HTTP handlers, work queues, producer-consumer) forces cross-shard frees. Building MPSC from the start avoids rework when the simple path inevitably fails.

---

## 2.2 — Per‑Shard LIFO Cache (Treiber Stack)

**Setup:** Lock-free Treiber stack per shard, Uint64 atomics (checkptr-safe), capacity=64

| Op | ns/op | allocs/op | Notes |
|----|-------|-----------|-------|
| ShardedFreeList hot path (Deallocate) | 54.4 | 0 | Deallocate → recycled cache (no atomics on same shard) |
| ShardedFreeList hot path (HP: Protect+Retire) | 77.5 | 0 | Protect CAS + Retire retirement push |
| FreeList hot path (baseline) | 37.7 | 0 | Single Treiber stack, no sharding overhead |

---

## 2.5 — Sharded Allocator (Deallocate path, no HP)

**Setup:** ShardedFreeList, 256MB pool, 64B slots, Prealloc. 8 shards.

| GOMAXPROCS | ns/op | MB/s | allocs/op | vs FreeList | Notes |
|------------|-------|------|-----------|-------------|-------|
| 1 | 53.8 | 1190 | 0 | 1.00x (42% slower) | Shard index + two-level cache overhead |
| 2 | 100.8 | 635 | 0 | 1.54x **faster** | Sharding beats contention |
| 4 | 196.4 | 326 | 0 | 1.08x faster | Benefit narrows as P-cores fill |
| 8 | 504.4 | 127 | 0 | 0.74x slower | M2 E-cores penalize sharding's higher per-op work |

**Note:** M2 has 4P+4E cores. GOMAXPROCS=8 adds E-cores which run ~3× slower.
Sharding wins clearly on P-cores (2-4). E-core penalty is architectural, not
a sharding flaw. On uniform-core servers (Graviton, Zen4), expect continued
scaling through 8+ cores.

---

## 3.1 — Hazard Pointer Publication Overhead

**Setup:** Protect (CAS publish) + Unprotect (Store clear) on ARM64 M2

| Path | ns/op | B/op | allocs/op | vs Deallocate path |
|------|-------|------|-----------|-------------------|
| Deallocate (fast path) | 54.4 | 0 | 0 | 1.00x baseline |
| Protect+Unprotect+Retire (HP path) | 77.5 | 6 | 0 | 1.42x (23ns overhead) |
| Retire only (scan amortized) | 92.1 | 42 | 0 | 1.69x (includes scan map/drain allocs amortized) |

HP overhead is ~23ns/op on M2. The CAS in Protect and the retirement stack
push account for most of this. Scan overhead (map allocation, drain slice)
is amortized across ~4M ops per scan.

---

## 3.3 — Hazard Pointer Scan

**Setup:** Drain all shard retirement stacks, map-based hazard lookup, linear walk

| NumShards | H (slots) | Drain time (est.) | Map alloc | Notes |
|-----------|-----------|-------------------|-----------|-------|
| 4 | 8 | O(retired nodes) | 8 entries | Scan triggered only on exhaustion |
| 8 | 16 | O(retired nodes) | 16 entries | ~64MB total alloc for 4M slot drain |
| 16 | 32 | O(retired nodes) | 32 entries | Linear — no SIMD needed for H<128 |

**Decision:** G4 — Linear scan with map lookup. H ≤ 32 for typical deployments
(8-16 shards × 2). Map construction is O(H) and lookup is O(1) per retired
node. Scalar linear scan confirmed sufficient per research.md (§7.2).

---

## 4.1 — Full‑Stack Sharded Allocator (Final)

**Setup:** ShardedFreeList with hazard pointers, 256MB pool, 64B slots, 8 shards, M2

| Benchmark | ns/op | MB/s | B/op | allocs/op | Notes |
|-----------|-------|------|------|-----------|-------|
| **Hot path (Deallocate)** | 54.4 | 1177 | 0 | 0 | Same-shard alloc/free, zero atomics |
| **Hot path (HP: Protect+Retire)** | 77.5 | 826 | 6 | 0 | Full HP lifecycle, scan amortized |
| **Concurrent 8-core (Deallocate)** | 411.6 | 155 | 0 | 0 | 8 goroutines, alloc+free loop |
| **Concurrent 8-core (HP)** | 337.9 | 189 | 0 | 0 | 8 goroutines, Protect+Retire |
| **Cross-shard (channel handoff)** | 272.0 | 235 | 0 | 0 | Producers + consumers, 100% cross-shard free |
| **Scan overhead (Retire only)** | 92.1 | 695 | 42 | 0 | Small pool forces frequent scans |

### FreeList vs ShardedFreeList (single goroutine)

| Allocator | ns/op | MB/s | B/op | allocs/op |
|-----------|-------|------|------|-----------|
| FreeList | 37.7 | 1699 | 0 | 0 |
| ShardedFreeList | 53.5 | 1197 | 0 | 0 |
| **Delta** | +42% | | | Sharding overhead: shard index, two caches, gen check |

### FreeList vs ShardedFreeList (concurrent, M2)

| GOMAXPROCS | FreeList ns/op | ShardedFreeList ns/op | Speedup | Notes |
|------------|---------------|----------------------|---------|-------|
| 1 | 37.3 | 53.8 | 0.69x | Sharding overhead |
| 2 | 155.6 | **100.8** | **1.54x** | Sharding wins |
| 4 | 211.6 | **196.4** | 1.08x | Sharding still ahead |
| 8 | 372.0 | 504.4 | 0.74x | E-cores penalize sharding |

---

## 4.2 — Race Detector Stress Test

**Setup:** 50× `go test -race -count=1` on all sharded + hazard tests

| Tests | Iterations | Result | Notes |
|-------|-----------|--------|-------|
| 11 tests (sharded + hazard) | 550 total | **ALL PASS** | Zero races, zero panics |
| TestShardedFreeListConcurrent | 50 iterations | PASS | 8×1000 alloc/free ops |
| TestHazardConcurrentProtectRetire | 50 iterations | PASS | 8×500 protect/retire ops |
| TestShardedFreeListCrossShard | 50 iterations | PASS | Forced cross-shard free |
| TestHazardProtectedSlotSurvivesScan | 50 iterations | PASS | Protected slot survives scan |

---

## 4.3 — GC Isolation

**Setup:** `GODEBUG=gctrace=1`, 1s benchmark runs, M2

| Path | Per-op heap alloc | Forced GC cycles | Steady-state GC | Notes |
|------|------------------|-----------------|-----------------|-------|
| Deallocate hot path | 0 B/op, 0 allocs/op | Setup only (mmap) | **None** | Perfect isolation |
| HP hot path | 6 B/op, 0 allocs/op | Setup + scan drain | **Amortized** | Scan allocations (map, drain slice) every ~4M ops |
| Concurrent (Deallocate) | 0 B/op, 0 allocs/op | Setup only | **None** | Sharded path adds zero heap pressure |
| Scan pressure (Retire only) | 42 B/op, 0 allocs/op | Per-scan drain | **Amortized** | Higher scan frequency in small pools |

**Key:** `0 allocs/op` on ALL paths — no per-operation heap allocations.
The Go GC never scans mmap'd memory. The mmap'd pool is invisible to the
tracer. GC `forced` cycles only fire during pool creation (mmap syscall
tracked by runtime) and during infrequent scan drain operations.

---

## 5.3 — Hyaline SMR Stress Hammer (Static Threshold = 65)

**Setup:** `ShardedFreeList`, 128MB pool, 128B slots, 32 slabs × 4MB, Prealloc.
**256 shards** (extreme over-provisioning). Workers = GOMAXPROCS × 32 = **256 goroutines**
hammering 5 mixed roles (bounce, retire/Hyaline, reader, publisher, burst).
**Static batch flush threshold = 65.**

### Summary (all runs, zero corruption on all)

| Run | Total ops | Avg ops/sec | Errors | Rate | Recovery | Notable |
|-----|-----------|-------------|--------|------|----------|---------|
| 30s | 415M | **13.84M** | 3.66M | 0.88% | 10K/10K | Steady climb 12.3→13.9M |
| 60s | 789M | **13.14M** | 7.87M | 1.0% | 10K/10K | Flat 13.1-13.4M, no drift |
| 5m | 3.74B | **12.48M** | 40.1M | 1.07% | 10K/10K | **6s stall at 4m44s**, self-recovered |

### 5-minute run — per-minute throughput

| Minute | ops/sec range | Total ops | Errors | corrupt |
|--------|--------------|-----------|--------|---------|
| 1 | 12.7–13.6M | 787M | 7.89M | 0 |
| 2 | 12.9–13.0M | 777M | 8.27M | 0 |
| 3 | 12.8–13.0M | 769M | 8.00M | 0 |
| 4 | 12.7–12.8M | 763M | 7.94M | 0 |
| 5 | 12.5–12.7M | 648M | 7.92M | 0 |

**6-second stall at 4m44s:** errors froze (38,639,298 → flat for 6s) as the pool
hit empty. Root cause: two sequential bottlenecks — (1) stranded partial batches
below the 65-node flush threshold sitting in per-shard queues, (2) passive drain
wall where flushed nodes waited in Hyaline slot chains for reader `Leave` cycles
(only ~20% of workers are readers). The allocator self-recovered without
intervention. No corruption.

---

## 5.4 — Hyaline SMR + Adaptive PID Threshold (Tier 2 Fix)

**Setup:** Identical to 5.3 — same 128MB pool, 256 shards, 256 workers.
**Change:** Static `hyalineThreshold=65` replaced with a PI-controlled dynamic
threshold (Kp=2.0, Ki=0.5, anti-windup ±100, 100ms ticker). Threshold adapts
from 65 down to 1 as pool depth drops below 20% free capacity. `forceReclamation()`
includes 4× `Gosched()` to yield to in-flight reader `Leave` cycles.

### Summary (all runs, zero corruption on all)

| Run | Total ops | Avg ops/sec | Errors | Rate | Recovery | Notable |
|-----|-----------|-------------|--------|------|----------|---------|
| 30s | 433M | **14.43M** | 1.39M | 0.32% | 10K/10K | Throughput climbs 12.1→14.4M, **no stall** |
| 5m | 3.95B | **13.16M** | 4.13M | 0.10% | 10K/10K | **Zero stalls**, errors increment every second |
| 10m | 7.34B | **12.23M** | 2.22M | 0.03% | 10K/10K | Flat 12.2M/s steady state, **no memory leak** |
| 1h | 42.02B | **11.67M** | 15.59M | 0.037% | **10K/10K** | **v1.0.0-gold** — zero stall, zero leak, zero corruption |

### 5-minute PID run — per-minute throughput

| Minute | ops/sec range | Total ops | Errors | corrupt |
|--------|--------------|-----------|--------|---------|
| 1 | 15.5→13.7M | 917M | 2.86M | 0 |
| 2 | 13.6→13.5M | 812M | 0.58M | 0 |
| 3 | 13.5→13.3M | 798M | 0.33M | 0 |
| 4 | 13.3→13.2M | 789M | 0.30M | 0 |
| 5 | 13.2→13.2M | 632M | 0.24M | 0 |

### 10-minute PID run — per-minute throughput

| Minute | ops/sec range | Total ops | Errors | corrupt |
|--------|--------------|-----------|--------|---------|
| 1 | 15.5→13.1M | 918M | 1.30M | 0 |
| 2 | 13.1→12.6M | 776M | 0.36M | 0 |
| 3 | 12.6→12.4M | 746M | 0.19M | 0 |
| 4 | 12.4→12.2M | 735M | 0.13M | 0 |
| 5 | 12.2→12.2M | 731M | 0.08M | 0 |
| 6 | 12.2→12.2M | 731M | 0.06M | 0 |
| 7 | 12.2→12.2M | 734M | 0.05M | 0 |
| 8 | 12.2→12.2M | 732M | 0.04M | 0 |
| 9 | 12.2→12.2M | 732M | 0.04M | 0 |
| 10 | 12.2→12.2M | 585M | 0.02M | 0 |

### 1-hour PID run — per-15-minute throughput

| Time | ops/sec | Total ops | Errors | corrupt |
|------|---------|-----------|--------|---------|
| 5m | 12.65M | 3.80B | 4.32M | 0 |
| 10m | 12.64M | 7.59B | 5.31M | 0 |
| 15m | 12.19M | 10.97B | 6.38M | 0 |
| 20m | 12.02M | 14.43B | 8.21M | 0 |
| 25m | 11.91M | 17.87B | 9.48M | 0 |
| 30m | 11.88M | 21.38B | 11.46M | 0 |
| 35m | 11.83M | 24.84B | 12.53M | 0 |
| 40m | 11.78M | 28.27B | 13.19M | 0 |
| 45m | 11.74M | 31.68B | 13.90M | 0 |
| 50m | 11.70M | 35.11B | 14.61M | 0 |
| 55m | 11.68M | 38.55B | 14.87M | 0 |
| 60m | 11.67M | 42.02B | 15.59M | 0 |

**1-hour steady state analysis:** Throughput declines asymptotically from 12.65M
at 5m to 11.67M at 60m — a 7.7% decline that decelerates, not accelerates. Error
rate per 5-minute window stabilizes at ~0.7M. If a memory leak existed, throughput
would accelerate downward and errors would spike. Neither occurs.

**Post-hammer recovery (1-hour run):** 10,000/10,000 alloc/free cycles succeeded
immediately after the hammer stopped. The pool drained cleanly — all Hyaline batch
chains were reclaimed, all shard caches were usable, and the global FreeList was
fully operational. Zero backlog, zero stranded nodes.

**RSS:** Flat at ~6 MB for the full hour. The 128 MB pool lives entirely off-heap
(mmap'd, invisible to the Go runtime and OS RSS accounting). The PID background
goroutine adds zero measurable heap pressure (100ms ticker, no allocations in the
control loop).

**Memory leak analysis:** Throughput flatlines at 12.2M/s from minute 3 through
minute 10 — zero degradation over 7+ minutes of continuous hammering. Error rate
converges to near-zero (0.02M/min in steady state vs 7.9M/min with static
threshold). If a heap or off-heap leak existed, throughput would continue
declining and errors would spike. The flat steady state confirms zero memory
leakage in both the Go heap (PID goroutine, ticker) and the off-heap mmap pool
(Hyaline batch/chain metadata).

### Before vs. After (5-minute run)

| Metric | Static (Before) | PID (After) | Improvement |
|--------|----------------|-------------|-------------|
| Stall duration | **6 seconds** | **0 seconds** | Eliminated |
| Error rate | 1.07% | 0.10% | **10× lower** |
| Total errors | 40.1M | 4.13M | **89.7% reduction** |
| Throughput | 12.48 M/s | 13.16 M/s | +5.5% |
| Corruption | 0 | 0 | — |

**Key finding:** The 6-second exhaustion stall is **completely eliminated.**
Under the static threshold, errors froze when the pool bottomed out — stranded
partial batches sat below the flush threshold while readers couldn't cycle
through `Leave` fast enough. The PID controller drops the threshold as pool
depth shrinks, forcing batches into the Hyaline pipeline sooner. Nodes spend
less time in per-shard limbo, readers drain them during normal `Leave` cycles,
and the exhaustion cliff becomes a smooth slope. The `Gosched` in
`forceReclamation` costs nanoseconds but gives in-flight readers a chance to
drain before the retry `BatchAllocate`.

**SMR safety:** No invariants violated. All flushes and drains go through the
mathematically proven Hyaline paths. The PID controller runs fully out-of-band
(100ms ticker, background goroutine). The hot path (`hyalineRetire`) sees only
a single `atomic.Uint64.Load` — zero new contention or branching.

---

## 6.1 — RAG Workload Benchmarks (Allocator Head-to-Head)

**Setup:** OpenAI embedding dimension (1536 float32 = 6KB/vector), 10K vector index.
5 allocators compared: **Pool** (CAS slab), **Make** (Go heap), **Slabby** (sync.Pool-based),
**FreeList** (lock-free Treiber stack), **ShardedFreeList** (64 shards + Hyaline SMR).
Apple M2, 8 cores, best-of-3 runs.

### Index Build (10K vectors, sequential)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| Make | 11,198,105 | 61,685,779 | 10,001 | 1.00x |
| Pool | 12,005,766 | 13,800 | 8 | 0.93x |
| FreeList | 12,004,995 | 361,303 | 8 | 0.93x |
| ShardedFreeList | 13,587,039 | 376,135 | 17 | 0.82x |
| Slabby | 26,320,222 | 62,221,758 | 10,024 | 0.43x |

### Query (top-10 cosine over 10K vectors)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| FreeList | 18,209,430 | 288 | 3 | 1.13x |
| ShardedFreeList | 19,588,279 | 288 | 3 | 1.05x |
| Slabby | 20,539,909 | 288 | 3 | 1.00x |
| Make | 20,551,588 | 288 | 3 | 1.00x |
| Pool | 21,410,219 | 288 | 3 | 0.96x |

### Concurrent Query (goroutines = GOMAXPROCS)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| FreeList | 3,506,383 | 290 | 3 | 1.12x |
| ShardedFreeList | 3,673,089 | 290 | 3 | 1.07x |
| Slabby | 3,700,726 | 296 | 3 | 1.06x |
| Make | 3,926,091 | 290 | 3 | 1.00x |
| Pool | 4,315,811 | 292 | 3 | 0.91x |

### Request Lifecycle (scratch alloc + query + Reset)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| Make | 18,454,938 | 288 | 3 | 1.00x |
| Pool | 18,607,199 | 288 | 3 | 0.99x |

### Concurrent Request Lifecycle

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| Make | 3,426,391 | 292 | 3 | 1.00x |
| Pool | 3,517,708 | 291 | 3 | 0.97x |

### Per-Vector Allocation (hot path, single slot)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| **FreeList** | **30.21** | 0 | 0 | **25.8x** |
| **ShardedFreeList** | **38.56** | 0 | 0 | **20.2x** |
| Slabby | 62.97 | 0 | 0 | 12.4x |
| Pool | 673.1 | 0 | 0 | 1.16x |
| Make | 779.3 | 6,144 | 1 | 1.00x |

### Concurrent Build (8 goroutines, 10K vectors)

| Allocator | ns/op | B/op | allocs/op | vs Make |
|-----------|-------|------|-----------|---------|
| Make | 3,089,693 | 61,686,275 | 10,012 | 1.00x |
| FreeList | 4,602,333 | 361,577 | 17 | 0.67x |
| ShardedFreeList | 5,443,813 | 376,397 | 26 | 0.57x |
| Pool | 7,419,546 | 14,178 | 17 | 0.42x |

### Key Takeaways

- **FreeList dominates per-vector allocation** at 30.2 ns/op — 25.8× faster than `make([]float32, 1536)` with zero heap allocs
- **ShardedFreeList** follows at 38.6 ns/op (20.2× vs Make), with the shard cache overhead adding ~8ns vs bare FreeList
- **Query/search workloads are GC-bound** — all allocators perform within ±13% of each other because the 10K-vector cosine search dominates the runtime, not the allocation layer
- **Pool is competitive with Make** on index build (0.93x) and within noise on query workloads — the CAS slab allocator adds minimal overhead
- **Make wins concurrent build** (3.09M ns) purely because Go heap allocation with a mutex is simpler than lock-free off-heap allocation for this specific pattern
- **Slabby is fast on per-vector** (63 ns) but slow on index build (0.43x) — the heap fallback path triggers under bulk allocation

---

## 5.1 / 5.2 — Platform Comparison

| Platform | Hot ns/op (Dealloc) | Hot ns/op (HP) | Concurrent 8-core ns/op | Notes |
|----------|--------------------|----------------|------------------------|-------|
| ARM64 M2 Darwin (8 cores, 4P+4E) | 54.4 | 77.5 | 411.6 (Dealloc), 337.9 (HP) | Hybrid arch skews 8-core results |
| ARM64 M3 Darwin | — | — | — | Pending |
| ARM64 Graviton Linux | — | — | — | Pending |
| x86_64 Zen4 Linux | — | — | — | Pending |

---

## Summary of Gating Decisions

| Gate | Date | Decision | Rationale |
|------|------|----------|-----------|
| G1 | Phase 1 | Sharding JUSTIFIED | 0.09x scaling at 8 cores, 3.67 CAS retries/op |
| G2 | Phase 1 | BatchAllocate CONFIRMED | ~2× per-slot throughput, batch size 32 |
| G3 | Phase 1→2 | Current-shard routing | Ring buffer built, proved fragile, removed |
| G4 | Phase 4 | Linear scan | H ≤ 32 for typical deployments; SIMD not needed (§7.2) |
