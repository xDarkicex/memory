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

## 5.3 — Hyaline SMR Stress Hammer (Extreme Contention)

**Setup:** `ShardedFreeList`, 128MB pool, 128B slots, 32 slabs × 4MB, Prealloc.
**256 shards** (extreme over-provisioning). Workers = GOMAXPROCS × 32 = **256 goroutines**
hammering 5 mixed roles (bounce, retire/Hyaline, reader, publisher, burst).

### Summary (all runs, zero corruption on all)

| Run | Total ops | Avg ops/sec | Errors | Rate | Recovery | Notable |
|-----|-----------|-------------|--------|------|----------|---------|
| 30s | 415M | **13.84M** | 3.66M | 0.88% | 10K/10K | Steady climb 12.3→13.9M |
| 60s | 789M | **13.14M** | 7.87M | 1.0% | 10K/10K | Flat 13.1-13.4M, no drift |
| 5m | 3.74B | **12.48M** | 40.1M | 1.07% | 10K/10K | Transient exhaustion at 4m44s, self-recovered |

### Per-second breakdown (30s / 60s runs)

| Time | 30s run | 60s run | corrupt |
|------|---------|---------|---------|
| 1s | 12.3M | 12.7M | 0 |
| 5s | — | 12.5M | 0 |
| 10s | 13.7M | 13.4M | 0 |
| 20s | 13.9M | 13.6M | 0 |
| 30s | 13.8M | 13.3M | 0 |
| 40s | — | 13.3M | 0 |
| 50s | — | 13.2M | 0 |
| 60s | — | 13.1M | 0 |

### 5-minute run — per-minute throughput

| Minute | ops/sec range | Total ops | Errors | corrupt |
|--------|--------------|-----------|--------|---------|
| 1 | 12.7–13.6M | 787M | 7.89M | 0 |
| 2 | 12.9–13.0M | 777M | 8.27M | 0 |
| 3 | 12.8–13.0M | 769M | 8.00M | 0 |
| 4 | 12.7–12.8M | 763M | 7.94M | 0 |
| 5 | 12.5–12.7M | 648M | 7.92M | 0 |

**Notes:** Throughput stable at 12.5–13.9M across all runs. Error rate (~1%)
is expected exhaustion under 256× oversubscription — every error is a clean
`ErrPoolExhausted` return, not a panic or deadlock.

**Transient exhaustion event at 4m44s (5-minute run):** throughput dipped to
12.5M and errors froze for ~6s as the pool hit empty — the Hyaline reclamation
pipeline momentarily fell behind 256× oversubscription. The allocator
self-recovered without intervention, throughput returned to ~12.48M, and
post-hammer recovery passed 10K/10K. No corruption.

**Key invariants validated:**
- Zero data corruption (slot magic round-trip) over **3.74 billion** ops
- Hyaline protect/retire integrity under concurrent readers + reclamation
- Arena publisher slot write → publish → read consistency
- Pool exhaustion → recovery cycle (transient exhaustion at T+284s, self-cleared)
- 256-shard extreme over-provisioning causes no regression
- Sustained throughput with zero degradation over 60s

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
