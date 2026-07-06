# OffHeapMap Architecture

The `OffHeapMap` is a specialized, zero-allocation wait-free hash map engineered specifically for the highly concurrent routing and indexing requirements of `libravdb`. It synthesizes several state-of-the-art algorithms into a single structure:

## 1. SWAR Probing (Swiss Tables / F14)
Drawing from the architectural breakthroughs of **Google's Swiss Tables (Abseil)** and **Facebook's F14**, the map splits 64-bit hashes into an index component and an H2 fingerprint. 
Instead of checking keys sequentially, the bucket metadata is packed into 64-bit words, allowing the engine to execute **SIMD Within A Register (SWAR)**. A single bitwise operation can simultaneously evaluate 8 slots for a fingerprint match in ~2 CPU cycles, bypassing branch misprediction penalties and enabling microsecond read/write latencies.

## 2. Wait-Free Cooperative Migration (Cliff Click)
Standard concurrent maps rely on `sync.RWMutex`, which suffers from catastrophic cache-line bouncing under heavy reader contention. 
`OffHeapMap` is completely lockless, utilizing a wait-free state machine inspired by **Dr. Cliff Click's Non-Blocking Hash Map**. When a table requires resizing, it allocates the next generation and enters a "migration" phase. Any thread that encounters a full table or a migration-in-progress actively jumps in to help migrate blocks of buckets concurrently using `atomic.CompareAndSwap` before completing its own operation. This completely eliminates "stop-the-world" resize latency spikes.

## 3. Adaptive PID Controller
To prevent resize cascades and memory exhaustion during extreme burst workloads, the map utilizes a background proportional-integral-derivative (PID) controller. It actively tracks insertion velocity and dynamically scales the load-factor threshold. If insertion rates spike beyond the hardware's ability to safely migrate, it applies gentle backpressure, ensuring the map remains stable under infinite oversubscription.

## 4. Zero-GC Off-Heap Layout
By storing raw `uint64` keys and values in dynamically mapped `mmap` regions, the map completely blinds the Go Garbage Collector to its contents. You can store 100 million routing pointers without adding a single millisecond to your GC scan time.