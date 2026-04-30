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

## 2.2 — Per‑Shard LIFO Cache

**Setup:** Per‑shard LIFO array, capacity=64, single goroutine

| Op | ns/op | allocs/op | Notes |
|----|-------|-----------|-------|
| LIFO push+pop (hot path) | | | |
| LIFO underflow + BatchPop refill | | | |
| Remote queue drain | | | |

---

## 2.5 — Sharded Allocator (Before Hazard Pointers)

**Setup:** ShardedFreeList with LIFO caches, batch‑pop, remote queues

| GOMAXPROCS | ops/sec | ns/op | allocs/op | vs baseline FreeList |
|------------|---------|-------|-----------|---------------------|
| 1 | | | | |
| 8 | | | | |
| 16 | | | | |
| 32 | | | | |
| 64 | | | | |

---

## 3.1 — Hazard Pointer Publication Overhead

**Setup:** atomic.StorePointer + atomic.LoadPointer on ARM64 vs x86_64

| Platform | ns/op (publish+validate+clear) | Notes |
|----------|--------------------------------|-------|
| ARM64 (M2) | | |
| x86_64 (Zen4) | | |

---

## 3.3 — Hazard Pointer Scan

**Setup:** Flat linear scan over hazard pointer snapshot

| NumShards | H (slots) | R (retired) | Scan time (ns) | ns/reclaimed_node | Notes |
|-----------|-----------|-------------|----------------|-------------------|-------|
| 16 | 32 | 64 | | | |
| 32 | 64 | 96 | | | |
| 64 | 128 | 160 | | | |
| 128 | 256 | 288 | | | |

**Decision:** G4 — (linear scan vs sort+binary search vs SIMD)

---

## 4.1 — Full‑Stack Sharded Allocator (Final)

**Setup:** ShardedFreeList with hazard pointers, full pipeline

| Benchmark | ops/sec | ns/op | allocs/op | vs baseline FreeList | vs Pool |
|-----------|---------|-------|-----------|---------------------|---------|
| Hot path (1 goroutine) | | | | | |
| 8 goroutines | | | | | |
| 16 goroutines | | | | | |
| 32 goroutines | | | | | |
| 64 goroutines | | | | | |
| Cross‑shard stress | | | | | |

---

## 4.3 — GC Isolation

**Setup:** `GODEBUG=gctrace=1`, sustained 30s run

| Path | GC Cycles | Auto GC | Live Heap | Notes |
|------|-----------|---------|-----------|-------|
| Hot path (1 goroutine) | | | | |
| 64 goroutines | | | | |
| Reset storm | | | | |
| Scan pressure | | | | |

---

## 5.1 / 5.2 — Platform Comparison

| Platform | Hot ns/op | 64‑goroutine ns/op | HP scan (ns) | Notes |
|----------|-----------|--------------------|--------------|-------|
| ARM64 M2 Darwin | | | | |
| ARM64 M3 Darwin | | | | |
| ARM64 Graviton Linux | | | | |
| x86_64 Zen4 Linux | | | | |
| x86_64 Sapphire Rapids Linux | | | | |

---

## Summary of Gating Decisions

| Gate | Date | Decision | Rationale |
|------|------|----------|-----------|
| G1 | | | |
| G2 | | | |
| G3 | | | |
| G4 | | | |
