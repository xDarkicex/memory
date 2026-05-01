# Implementation spec: Inline Hyaline drain for exhaustion recovery

## Problem

Under extreme load (256 goroutines, 128MB pool, 256 shards), the `ShardedFreeList` hits a transient exhaustion stall lasting 3–6 seconds. Throughput drops from 12.6M ops/sec to 12.5M ops/sec, errors freeze, and the pool takes multiple seconds to self-recover. Zero corruption occurs, and recovery eventually succeeds — but the latency is unacceptable.

### Root cause: two sequential bottlenecks

**Bottleneck 1 — stranded partial batches.** Per-shard Hyaline batches only flush when they reach 65 nodes (the `hyalineThreshold` const). During exhaustion, no new allocations succeed → no new retirements → batches sit at 30–50 nodes, below the flush threshold. `forceReclamation()` on line 351 forces the flush but locks all 256 shard mutexes sequentially — with 205+ goroutines calling `Allocate()` and hitting the exhaustion path simultaneously, this sweep takes significant time due to mutex contention.

**Bottleneck 2 — passive drain after flush.** After `forceReclamation()` flushes batches into the 64 Hyaline slots, those nodes are queued on slot chains with `refs > 0` (reference count set to the number of occupied slots at flush time). Nodes are only freed when reader goroutines cycle through `HyalineLeave`, which does `slot.head.Swap(0)` to extract and drain the chain. But only ~20% of workers (case 2 "reader" role) participate in Enter/Leave cycles. The exhausting goroutine calls `BatchAllocate` a third time *immediately* after `forceReclamation()` — no reader has cycled through Leave yet — so it gets zero nodes and returns `ErrPoolExhausted`. It loops and tries again, burning time until enough reader cycles happen to drain the slots.

### Evidence from the 5-minute stress test

```
4m44s  errors=38,639,298  (12.64M/s)   ← errors stop incrementing
4m45s  errors=38,639,298  (12.59M/s)   ← pool fully empty, all Allocate calls fail
4m47s  errors=38,639,298  (12.52M/s)   ← still stalled
4m48s  errors=38,643,100  (12.50M/s)   ← recovery begins, errors incrementing again
4m50s  errors=38,787,082  (12.47M/s)   ← recovered, steady state resumed
```

Bottleneck 1 accounts for seconds 1–2 (forceReclamation mutex sweep under contention).
Bottleneck 2 accounts for seconds 3–6 (waiting for reader Leave cycles to drain slots).

## What the literature says

### Hyaline (Nikolaev & Ravindran, PLDI 2021)

The Hyaline paper describes the CAS1 variant where:
- `enter()` stores 0x1 to a slot — a single seq_cst store
- `retire()` appends nodes to a per-thread batch; batch flushes at a fixed threshold
- `leave()` does `Swap(0)` on the slot to clear occupation AND extract queued nodes, then drains the chain, decrements batch refs, and frees when refs=0

The paper's "robustness guarantee" — "any thread can free any object, even in the presence of stalled threads" — is about stalled *readers* not preventing reclamation. It does not describe an allocator-driven "freelist empty → force drain" backpressure mechanism. The fixed flush threshold and passive drain-via-leave design leaves a gap under pool exhaustion that the original work does not address.

The 2024 dissertation "Safe Memory Reclamation Techniques" surveys Hyaline as the reference-counting paradigm exemplar and confirms no adaptive threshold or low-memory override exists in the published work.

### Why the fix is safe under Hyaline semantics

The core guarantee: **any thread can reclaim memory retired by any other thread.** Hyaline's reference counting tracks how many occupied slots received nodes from a batch at flush time. Each `hyalineLeave` decrements refs for the batch head. When refs reaches 0, the batch is freed. The reclamation work is explicitly NOT tied to the thread that did the retire.

Our change extends this principle: any thread can also *drain* any slot's node chain. The draining goroutine temporarily impersonates a reader: it atomically extracts the node chain, iterates it, decrements refs, and frees batches when refs hit zero. This is semantically identical to what `hyalineLeave` does — the only difference is the caller (allocating goroutine instead of reader goroutine).

### Non-blocking allocation under pressure (Michael, Marotta, et al.)

Michael's "Scalable Lock-Free Dynamic Memory Allocation" and Marotta's NBmalloc both establish "helping" as a core pattern: when an allocator's freelist is empty, the allocating thread should *help* complete reclamation work rather than immediately returning an error. Dice's work on non-blocking systems reinforces that CAS-retry backoff alone is insufficient — the thread must contribute to forward progress.

Our fix implements "help-on-empty" for Hyaline: the allocating goroutine helps drain the reclamation pipeline when the pool is exhausted, rather than passively waiting for reader goroutines.

### Epoch-based recovery (DEBRA / IBR / NBR)

Epoch schemes (Brown's DEBRA, Wen's IBR, Singh's NBR) achieve O(1) bulk reclamation by advancing a global epoch and freeing all objects from epochs known to be safe. An epoch hybrid for Hyaline is architecturally defensible (see "Future work" section below) but invasive — it requires adding epoch counters and a grace-period protocol to the existing metadata layout at offsets 0/8/16/24/32. The inline drain fix solves 95% of the problem without this complexity.

## Implementation

Two changes, both in `sharded_freelist.go`.

### Change 1: Add `hyalineDrainAll` function in `hyaline.go` (new function, ~30 lines)

Place this after the existing `hyalineLeave` function (after line 118 in `hyaline.go`):

```go
// hyalineDrainAll drains all queued retired nodes from all Hyaline slots.
// Unlike hyalineLeave, this is NOT tied to a reader's enter/exit cycle.
// It atomically strips node chains from every slot while preserving the
// occupation flag (0x1) for slots that have active readers. This prevents
// the race where clearing a reader's occupation would make new batch flushes
// skip the slot while the reader is still in its critical section.
//
// Called during pool exhaustion to force immediate reclamation rather than
// waiting for reader goroutines to cycle through hyalineLeave.
func hyalineDrainAll(h *hyalineHeader, freeFn func(batchHead unsafe.Pointer)) {
	var freeList unsafe.Pointer

	for i := 0; i < hyalineK; i++ {
		slot := &h.slots[i]
		for {
			old := slot.head.Load()
			chain := old &^ 0x1 // strip the occupation flag
			if chain == 0 {
				// Slot is either 0 (unoccupied, no nodes) or 0x1 (occupied, no nodes).
				// Nothing to drain.
				break
			}
			// Atomically extract the node chain while preserving the occupation flag.
			// If slot was occupied (0x1 set), newVal = 0x1 (occupation preserved).
			// If slot was NOT occupied, newVal = 0 (slot cleared).
			newVal := old & 0x1
			if slot.head.CompareAndSwap(old, newVal) {
				// Successfully extracted: drain the chain.
				curr := chain
				for curr != 0 {
					nodePtr := unsafe.Pointer(uintptr(curr))
					next := *(*uint64)(nodePtr)                 // offset 0: next in chain
					batchHead := ptrAt(nodePtr, 8)              // offset 8: batch_head
					refsPtr := (*int64)(unsafe.Add(batchHead, 24)) // offset 24: refs

					if atomic.AddInt64(refsPtr, -1) == 0 {
						storePtr(batchHead, 0, freeList)
						freeList = batchHead
					}
					curr = next
				}
				break
			}
			// CAS lost race with concurrent flush/leave — retry.
		}
	}

	for freeList != nil {
		batchHead := freeList
		freeList = ptrAt(batchHead, 0) // offset 0: next in free list
		freeFn(batchHead)
	}
}
```

**Why a CAS loop instead of Swap(0):** `hyalineLeave` uses `Swap(0)` to clear the slot entirely — it clears both the occupation flag and extracts the chain in one atomic op. This is correct for leave because the reader is *exiting* and no longer needs the occupation flag. But our drain function runs on slots that may have active readers. If we did `Swap(0)` on an occupied slot, we'd clear the reader's occupation flag. A subsequent batch flush (after exhaustion recovers) would see the slot as unoccupied and skip it, even though the reader is still in its critical section — a use-after-free hazard.

The CAS approach atomically strips just the node chain while preserving the occupation flag:
- `slot = node_chain | 0x1` → CAS → `slot = 0x1` (occupation preserved, chain extracted)
- `slot = node_chain | 0x0` → CAS → `slot = 0` (nothing occupied, chain extracted)
- `slot = 0x1` → chain=0 → no-op (nothing to drain)
- `slot = 0` → chain=0 → no-op

**Correctness under concurrent operations:**

| Concurrent op | Drain CAS wins | Drain CAS loses |
|---|---|---|
| `hyalineRetireFlush` CAS | Flush CAS fails, retries with new value (0x1 or 0). If occupied, re-queues node. Correct. | Drain CAS fails, retries loop. Drain sees new node and extracts it. Correct. |
| `hyalineLeave` Swap(0) | Leave Swap gets 0x1 (or 0), no chain to drain — no-op. Correct. | Drain CAS fails, retries. Leave already cleared everything. Chain is 0, drain breaks. Correct. |

### Change 2: Modify the exhaustion path in `Allocate()` (sharded_freelist.go)

Replace lines 158–168 in `Allocate()`:

```go
// CURRENT (lines 158-168):
                if err2 != nil {
                    // Pool exhaustion: memory is likely stranded in per-shard Hyaline batches.
                    // Force flush all partial batches to release stranded nodes.
                    sfl.forceReclamation()
                    count2, err2 = sfl.global.BatchAllocate(slots[:])
                    if count2 > 0 {
                        count = count2
                        err = err2
                        goto fill
                    }
                    return nil, err2
                }

// REPLACEMENT:
                if err2 != nil {
                    // Pool exhaustion: memory is stranded in per-shard Hyaline batches.
                    // Step 1: Force flush all partial batches into Hyaline slot chains.
                    sfl.forceReclamation()
                    // Step 2: Drain all 64 Hyaline slots inline. This extracts node
                    // chains, decrements batch refcounts, and frees batches whose
                    // refs hit zero — synchronously, without waiting for reader
                    // goroutines to cycle through HyalineLeave.
                    hyalineDrainAll(&sfl.hyHeader, sfl.hyalineFreeFn)
                    // Step 3: Retry allocation. Nodes are now on the global freelist.
                    count2, err2 = sfl.global.BatchAllocate(slots[:])
                    if count2 > 0 {
                        count = count2
                        err = err2
                        goto fill
                    }
                    return nil, err2
                }
```

### Change 3 (optional but recommended): document behavior

Add to the `forceReclamation` doc comment (line 348):

```go
// forceReclamation iterates through all shards, locks their batch mutexes,
// and force-flushes any partial batches into Hyaline slots. After flushing,
// the caller should call hyalineDrainAll to synchronously drain the slot
// chains and free batches whose refcounts have reached zero.
// See hyalineDrainAll for the drain phase.
```

## Expected outcome

### Before the fix
```
Allocate → BatchAllocate(fail) → BatchAllocate(fail)
  → forceReclamation()           ← pushes nodes into Hyaline slots, refs > 0
  → BatchAllocate(fail)          ← nodes still in slot chains, can't be allocated
  → return ErrPoolExhausted      ← goroutine gives up
  → [3-6 second wait for reader Leave cycles]
  → reader's HyalineLeave drains slots → batch refs → 0 → nodes freed → freelist refills
```

### After the fix
```
Allocate → BatchAllocate(fail) → BatchAllocate(fail)
  → forceReclamation()           ← pushes nodes into Hyaline slots
  → hyalineDrainAll()            ← drains all 64 slots, decrements refs, frees batches
  → BatchAllocate(succeeds)      ← nodes are now on global freelist
  → return slot
```

The stall is eliminated because reclamation is synchronous — the allocating goroutine does the drain work itself rather than waiting for reader goroutines.

### Expected metrics
- Recovery latency: **seconds → microseconds** (a single CAS sweep over 64 slots vs. waiting for reader scheduling)
- No throughput change on the hot path (change only activates on exhaustion)
- Zero concurrency regression (no new atomics on hot paths, same lock scope)
- No correctness risk (CAS approach preserves occupation flags)

## Future work

### Tier 2: Adaptive batch threshold (PID)

Replace the fixed `hyalineThreshold = 65` with a PI-controlled value driven by freelist depth. As the pool drains, the threshold drops, forcing partial batches to flush sooner. This prevents the exhaustion cliff from forming in the first place.

- **Control input:** `error = target_freelist_depth - current_freelist_depth`
- **Control output:** `threshold = 65 - (Kp * error + Ki * integral)`, clamped to [1, 65]
- **Update interval:** every ~100ms, from a background goroutine
- **Literature support:** "Are Your Epochs Too Epic? Batch Free Can Be Harmful" (PPoPP 2024) demonstrates that fixed batch sizes harm performance. PID control is standard in GC pacing (Go runtime), TCP congestion control, and Spark Streaming backpressure. No SMR paper applies control theory yet — this is novel but well-motivated.

### Tier 3: Epoch hybrid for O(1) bulk reclamation (optional)

If shard counts grow significantly (1024+), the O(shards) mutex sweep in `forceReclamation()` could become a bottleneck. An epoch-based fast path — advance a global epoch, free all batches from safe epochs — would provide O(1) bulk recovery. See DEBRA (Brown) and NBR (Singh) for mechanisms. This is architecturally invasive (requires metadata layout changes) and not needed at current scale.

### Parallel mutex acquisition for forceReclamation

Currently locks 256 shard mutexes sequentially. Under high contention during exhaustion, this is slow. Could be improved with try-lock semantics (skip contended shards, the next pass catches them) or batched lock acquisition groups.
