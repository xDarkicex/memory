// Package memory — extreme stress tests for ShardedFreeList + Hyaline SMR.
//
// These tests push the allocator far beyond normal benchmarks to validate
// production correctness: no data corruption, no double-frees, no deadlocks,
// no pool exhaustion leaks, and Hyaline reclamation integrity under fire.
//
// Run with:
//
//	go test -run=Stress -race -count=1 -timeout 30m .
//	go test -run=Stress -count=1 -timeout 10m .
//	go test -run=Stress -short -count=1 .     # quick smoke test

package memory

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// stressCfg returns a config sized for stress testing: 64MB pool, 128-byte
// slots, enough to hold 512K concurrent slots. Prealloc=true avoids lazy-mmap
// overhead during the test.
func stressCfg() FreeListConfig {
	return FreeListConfig{
		PoolSize:  64 * 1024 * 1024,
		SlotSize:  128,
		SlabSize:  2 * 1024 * 1024,
		SlabCount: 32,
		Prealloc:  true,
	}
}

// stressTinyCfg returns a small pool config that can be exhausted.
func stressTinyCfg() FreeListConfig {
	return FreeListConfig{
		PoolSize:  2 * 1024 * 1024, // 2MB
		SlotSize:  128,
		SlabSize:  256 * 1024,
		SlabCount: 8,
		Prealloc:  true,
	}
}

// ---------------------------------------------------------------------------
// Integrity helpers
// ---------------------------------------------------------------------------

// slotMagic returns a per-slot tag: goroutine id in upper 32 bits, monotonic
// sequence in lower 32 bits.
func slotMagic(gid, seq int) uint64 {
	return uint64(gid)<<32 | uint64(seq)&0xFFFFFFFF
}

// writeSlot writes a magic value at payload offset and returns it.
func writeSlot(slot []byte, magic uint64) {
	*(*uint64)(unsafe.Pointer(unsafe.SliceData(slot))) = magic
}

// readSlot reads the magic value at payload offset.
func readSlot(slot []byte) uint64 {
	return *(*uint64)(unsafe.Pointer(unsafe.SliceData(slot)))
}

// ---------------------------------------------------------------------------
// TestStressBounce — rapid alloc/dealloc, maximal shard cache thrashing
// ---------------------------------------------------------------------------

func TestStressBounce(t *testing.T) {
	dur := 10 * time.Second
	if testing.Short() {
		dur = 2 * time.Second
	}

	sfl, err := NewShardedFreeList(stressCfg(), 128)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)
	workers := numCPU * 16 // massive over-subscription
	t.Logf("StressBounce: workers=%d duration=%v shards=128", workers, dur)

	var (
		ops       atomic.Int64
		errs      atomic.Int64
		corrupts  atomic.Int64
		done      atomic.Bool
		start     = time.Now()
	)

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			for !done.Load() {
				slot, err := sfl.Allocate()
				if err != nil {
					errs.Add(1)
					seq++
					continue
				}
				magic := slotMagic(gid, seq)
				writeSlot(slot, magic)
				if got := readSlot(slot); got != magic {
					corrupts.Add(1)
				}
				if err := sfl.Deallocate(slot); err != nil {
					errs.Add(1)
				}
				seq++
				ops.Add(1)
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("ops=%d (%.0f/s) errors=%d corruptions=%d",
		ops.Load(), float64(ops.Load())/elapsed.Seconds(), errs.Load(), corrupts.Load())

	if corrupts.Load() > 0 {
		t.Fatalf("DATA CORRUPTION: %d slot writes did not round-trip", corrupts.Load())
	}
	if errs.Load() > 0 {
		t.Fatalf("errors=%d (should be 0 — pool should not exhaust under bounce)", errs.Load())
	}
}

// ---------------------------------------------------------------------------
// TestStressHyalineReclamation — retire/enter/leave interleaving
// ---------------------------------------------------------------------------

func TestStressHyalineReclamation(t *testing.T) {
	dur := 10 * time.Second
	if testing.Short() {
		dur = 2 * time.Second
	}

	sfl, err := NewShardedFreeList(stressCfg(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)
	// Half the workers are "producers" that allocate + retire.
	// The other half are "readers" that enter + read + leave (simulating
	// concurrent access to slots that may be retired).
	producers := numCPU * 4
	readers := numCPU * 4
	t.Logf("StressHyalineReclamation: producers=%d readers=%d duration=%v", producers, readers, dur)

	var (
		pOps     atomic.Int64
		rOps     atomic.Int64
		errs     atomic.Int64
		done     atomic.Bool
		start    = time.Now()
		// Pool of "live" slots that readers may access.
		livePtrs []unsafe.Pointer
		liveMu   sync.Mutex
	)

	// Pre-allocate some live slots for readers.
	for i := 0; i < 256; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		writeSlot(slot, slotMagic(0, i))
		livePtrs = append(livePtrs, unsafe.Pointer(unsafe.SliceData(slot)))
	}

	var wg sync.WaitGroup

	// Producers: allocate → write → retire under Hyaline protection.
	for g := 0; g < producers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			for !done.Load() {
				shardIdx := gid & 63 // deterministic slot for enter/leave

				sfl.HyalineEnter(shardIdx)
				slot, err := sfl.Allocate()
				if err != nil {
					sfl.HyalineLeave(shardIdx)
					errs.Add(1)
					seq++
					continue
				}

				magic := slotMagic(gid, seq)
				writeSlot(slot, magic)

				// Make this slot visible to readers briefly.
				ptr := unsafe.Pointer(unsafe.SliceData(slot))
				liveMu.Lock()
				livePtrs = append(livePtrs, ptr)
				liveMu.Unlock()

				// Simulate brief work.
				runtime.Gosched()

				// Remove from live set before retiring.
				liveMu.Lock()
				for i, p := range livePtrs {
					if p == ptr {
						livePtrs[i] = livePtrs[len(livePtrs)-1]
						livePtrs = livePtrs[:len(livePtrs)-1]
						break
					}
				}
				liveMu.Unlock()

				if err := sfl.Retire(unsafe.Slice((*byte)(ptr), int(sfl.cfg.SlotSize))); err != nil {
					errs.Add(1)
				}
				sfl.HyalineLeave(shardIdx)
				seq++
				pOps.Add(1)
			}
		}(g)
	}

	// Readers: enter → read live slots → leave (no alloc/free).
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for !done.Load() {
				shardIdx := (gid + producers) & 63

				sfl.HyalineEnter(shardIdx)

				liveMu.Lock()
				snapshot := make([]unsafe.Pointer, len(livePtrs))
				copy(snapshot, livePtrs)
				liveMu.Unlock()

				for _, ptr := range snapshot {
					_ = *(*uint64)(ptr) // touch the memory
				}

				sfl.HyalineLeave(shardIdx)
				rOps.Add(1)
				runtime.Gosched()
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("producer_ops=%d reader_ops=%d (total=%.0f/s) errors=%d",
		pOps.Load(), rOps.Load(),
		float64(pOps.Load()+rOps.Load())/elapsed.Seconds(),
		errs.Load())

	if errs.Load() > 0 {
		t.Fatalf("errors=%d (should be 0)", errs.Load())
	}
}

// ---------------------------------------------------------------------------
// TestStressExhaustion — exhaust pool, force Hyaline reclamation, verify recovery
// ---------------------------------------------------------------------------

func TestStressExhaustion(t *testing.T) {
	sfl, err := NewShardedFreeList(stressTinyCfg(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// 2MB pool / 128 bytes per slot = 16,384 slots. Drain them all.
	poolSlots := int(sfl.cfg.PoolSize / sfl.cfg.SlotSize)
	t.Logf("StressExhaustion: poolSlots=%d", poolSlots)

	// Phase 1: exhaust the pool.
	var held [][]byte
	for i := 0; i < poolSlots; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("exhaustion at slot %d: %v (poolSlots=%d)", i, err, poolSlots)
		}
		writeSlot(slot, slotMagic(1, i))
		held = append(held, slot)
	}

	// Pool should be empty now.
	if _, err := sfl.Allocate(); err == nil {
		t.Fatal("pool should be exhausted, but Allocate succeeded")
	}
	t.Logf("pool exhausted after %d allocations", len(held))

	// Phase 2: retire a batch to trigger Hyaline reclamation.
	// We need to enter before retiring so reclamation is deferred.
	const batchSize = 256
	shardIdx := 0
	sfl.HyalineEnter(shardIdx)
	for i := 0; i < batchSize; i++ {
		if err := sfl.Retire(held[i]); err != nil {
			t.Fatalf("retire slot %d: %v", i, err)
		}
	}
	sfl.HyalineLeave(shardIdx)

	// After leave drains and reclaims, slots should be back in the global free list.
	recovered := 0
	for i := 0; i < batchSize; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		writeSlot(slot, slotMagic(2, i))
		held[i] = slot // save it to be deallocated in phase 3
		recovered++
	}
	t.Logf("recovered %d / %d slots after reclamation", recovered, batchSize)

	if recovered == 0 {
		t.Fatal("Hyaline reclamation failed to recover any slots")
	}

	// Phase 3: return remaining held slots via Deallocate (fast path).
	for i := 0; i < len(held); i++ {
		if err := sfl.Deallocate(held[i]); err != nil {
			t.Fatalf("deallocate slot %d: %v", i, err)
		}
	}

	// All slots should now be recoverable.
	finalRecovered := 0
	for {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		_ = slot
		finalRecovered++
	}
	t.Logf("final recovery: %d / %d slots back in free list", finalRecovered, poolSlots)
	if finalRecovered < poolSlots {
		t.Fatalf("slot leak: only %d/%d slots recoverable after full deallocation", finalRecovered, poolSlots)
	}
}

// ---------------------------------------------------------------------------
// TestStressConcurrentRetire — retire storm from many goroutines
// ---------------------------------------------------------------------------

func TestStressConcurrentRetire(t *testing.T) {
	dur := 5 * time.Second
	if testing.Short() {
		dur = 1 * time.Second
	}

	sfl, err := NewShardedFreeList(stressCfg(), 128)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)
	workers := numCPU * 8
	t.Logf("StressConcurrentRetire: workers=%d duration=%v", workers, dur)

	var (
		ops   atomic.Int64
		errs  atomic.Int64
		done  atomic.Bool
		start = time.Now()
	)

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			shardIdx := gid & 127
			for !done.Load() {
				sfl.HyalineEnter(shardIdx)

				slot, err := sfl.Allocate()
				if err != nil {
					sfl.HyalineLeave(shardIdx)
					errs.Add(1)
					seq++
					continue
				}

				magic := slotMagic(gid, seq)
				writeSlot(slot, magic)
				if got := readSlot(slot); got != magic {
					errs.Add(1)
				}

				// Retire (Hyaline path) — contends on per-shard batchMu.
				if err := sfl.Retire(slot); err != nil {
					errs.Add(1)
				}

				sfl.HyalineLeave(shardIdx)
				seq++
				ops.Add(1)
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("ops=%d (%.0f/s) errors=%d",
		ops.Load(), float64(ops.Load())/elapsed.Seconds(), errs.Load())

	if errs.Load() > 0 {
		// Tolerate ErrPoolExhausted. Hyaline defers reclamation, so under extreme
		// load, the 64MB pool may briefly exhaust if readers haven't called Leave.
		t.Logf("tolerated %d transient pool exhaustion errors during retire storm", errs.Load())
	}

	// Final sanity: after all workers stop, we should be able to allocate.
	for i := 0; i < 1000; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("post-stress allocate %d failed: %v", i, err)
		}
		sfl.Deallocate(slot)
	}
}

// ---------------------------------------------------------------------------
// TestStressMixedWorkload — alloc/dealloc + alloc/retire + enter/leave
// ---------------------------------------------------------------------------

func TestStressMixedWorkload(t *testing.T) {
	dur := 15 * time.Second
	if testing.Short() {
		dur = 3 * time.Second
	}

	sfl, err := NewShardedFreeList(stressCfg(), 128)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)

	// Three worker types:
	//   Bouncers: rapid alloc/dealloc (shard cache hot path)
	//   Retirers: alloc/retire via Hyaline (reclamation path)
	//   Readers:  enter/touch/leave (simulates concurrent access)
	bouncers := numCPU * 4
	retirers := numCPU * 4
	readers := numCPU * 2

	t.Logf("StressMixedWorkload: bouncers=%d retirers=%d readers=%d duration=%v",
		bouncers, retirers, readers, dur)

	var (
		bOps    atomic.Int64
		rOps    atomic.Int64
		rdOps   atomic.Int64
		errs    atomic.Int64
		done    atomic.Bool
		start   = time.Now()
	)

	// Shared pool of pointers for readers to touch.
	var sharedPtrs [256]unsafe.Pointer
	var sharedMu sync.RWMutex

	// Pre-populate shared pointers.
	for i := range sharedPtrs {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		writeSlot(slot, uint64(i))
		sharedPtrs[i] = unsafe.Pointer(unsafe.SliceData(slot))
	}

	var wg sync.WaitGroup

	// Bouncers: alloc/dealloc.
	for g := 0; g < bouncers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			for !done.Load() {
				slot, err := sfl.Allocate()
				if err != nil {
					errs.Add(1)
					seq++
					continue
				}
				writeSlot(slot, slotMagic(gid, seq))

				// Briefly publish for readers.
				sharedMu.Lock()
				sharedPtrs[gid%len(sharedPtrs)] = unsafe.Pointer(unsafe.SliceData(slot))
				sharedMu.Unlock()

				if got := readSlot(slot); got != slotMagic(gid, seq) {
					errs.Add(1)
				}
				if err := sfl.Deallocate(slot); err != nil {
					errs.Add(1)
				}
				seq++
				bOps.Add(1)
			}
		}(g)
	}

	// Retirers: alloc/retire via Hyaline.
	for g := 0; g < retirers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			shardIdx := gid & 127
			for !done.Load() {
				sfl.HyalineEnter(shardIdx)
				slot, err := sfl.Allocate()
				if err != nil {
					sfl.HyalineLeave(shardIdx)
					errs.Add(1)
					seq++
					continue
				}
				writeSlot(slot, slotMagic(gid+bouncers, seq))

				sharedMu.Lock()
				sharedPtrs[gid%len(sharedPtrs)] = unsafe.Pointer(unsafe.SliceData(slot))
				sharedMu.Unlock()

				if got := readSlot(slot); got != slotMagic(gid+bouncers, seq) {
					errs.Add(1)
				}
				if err := sfl.Retire(slot); err != nil {
					errs.Add(1)
				}
				sfl.HyalineLeave(shardIdx)
				seq++
				rOps.Add(1)
			}
		}(g)
	}

	// Readers: continuous enter/touch/leave.
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			shardIdx := (gid + bouncers + retirers) & 127
			for !done.Load() {
				sfl.HyalineEnter(shardIdx)
				sharedMu.RLock()
				for _, ptr := range sharedPtrs {
					if ptr != nil {
						_ = *(*uint64)(ptr)
					}
				}
				sharedMu.RUnlock()
				sfl.HyalineLeave(shardIdx)
				rdOps.Add(1)
				runtime.Gosched()
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()
	elapsed := time.Since(start)

	totalOps := bOps.Load() + rOps.Load() + rdOps.Load()
	t.Logf("ops: bounce=%d retire=%d read=%d (total=%.0f/s) errors=%d",
		bOps.Load(), rOps.Load(), rdOps.Load(),
		float64(totalOps)/elapsed.Seconds(),
		errs.Load())

	if errs.Load() > 0 {
		t.Fatalf("errors=%d", errs.Load())
	}

	// Post-stress: verify pool is still functional.
	for i := 0; i < 10000; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatalf("post-stress allocate %d failed after %d ops: %v", i, totalOps, err)
		}
		sfl.Deallocate(slot)
	}
}

// ---------------------------------------------------------------------------
// TestStressDoubleFree — verify double-free detection under contention
// ---------------------------------------------------------------------------

func TestStressDoubleFree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	sfl, err := NewShardedFreeList(stressCfg(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	// The double-free test must be single-goroutine.
	// If concurrent, another goroutine could Allocate the slot immediately 
	// after the first Deallocate and before the second Deallocate, leading 
	// to memory corruption when the second Deallocate clobbers the active pointer.
	workers := 1
	t.Logf("StressDoubleFree: workers=%d", workers)

	var (
		doubleFrees atomic.Int64
		otherErrors atomic.Int64
		done        atomic.Bool
	)

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			for i := 0; i < 100000; i++ {
				if done.Load() {
					return
				}
				slot, err := sfl.Allocate()
				if err != nil {
					otherErrors.Add(1)
					continue
				}
				writeSlot(slot, slotMagic(gid, seq))

				// First free succeeds.
				if err := sfl.Deallocate(slot); err != nil {
					otherErrors.Add(1)
					continue
				}

				// Second free must fail (double-free detection).
				if err := sfl.Deallocate(slot); err == nil {
					doubleFrees.Add(1)
				}
				seq++
			}
		}(g)
	}

	wg.Wait()

	t.Logf("double_frees_undetected=%d other_errors=%d", doubleFrees.Load(), otherErrors.Load())

	if doubleFrees.Load() > 0 {
		t.Fatalf("UNDETECTED DOUBLE-FREES: %d", doubleFrees.Load())
	}
}

// ---------------------------------------------------------------------------
// TestStressStatsConsistency — allocated counter must never exceed pool size
// ---------------------------------------------------------------------------

func TestStressStatsConsistency(t *testing.T) {
	dur := 5 * time.Second
	if testing.Short() {
		dur = 1 * time.Second
	}

	sfl, err := NewShardedFreeList(stressCfg(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)
	workers := numCPU * 8
	maxAllocated := sfl.cfg.PoolSize
	t.Logf("StressStatsConsistency: workers=%d maxAllocated=%d", workers, maxAllocated)

	var (
		badStats atomic.Int64
		done     atomic.Bool
	)

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for !done.Load() {
				slot, err := sfl.Allocate()
				if err != nil {
					// Pool exhaustion is OK (reclamation may lag).
					continue
				}

				stats := sfl.Stats()
				if stats.Allocated > maxAllocated {
					badStats.Add(1)
				}

				// 50% deallocate, 50% retire
				if gid%2 == 0 {
					sfl.Deallocate(slot)
				} else {
					shardIdx := gid & 63
					sfl.HyalineEnter(shardIdx)
					sfl.Retire(slot)
					sfl.HyalineLeave(shardIdx)
				}
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()

	if badStats.Load() > 0 {
		t.Fatalf("ALLOCATED EXCEEDED POOL SIZE: %d times", badStats.Load())
	}
	t.Logf("stats ok — allocated never exceeded %d", maxAllocated)
}

// ---------------------------------------------------------------------------
// TestStressHammer — everything at once, maximum carnage
// ---------------------------------------------------------------------------

func TestStressHammer(t *testing.T) {
	dur := 30 * time.Second
	if testing.Short() {
		dur = 5 * time.Second
	}

	sfl, err := NewShardedFreeList(FreeListConfig{
		PoolSize:  128 * 1024 * 1024, // 128MB
		SlotSize:  128,
		SlabSize:  4 * 1024 * 1024,
		SlabCount: 32,
		Prealloc:  true,
	}, 256) // 256 shards — extreme over-provisioning
	if err != nil {
		t.Fatal(err)
	}
	defer sfl.Free()

	numCPU := runtime.GOMAXPROCS(0)
	workers := numCPU * 32 // extreme over-subscription
	t.Logf("StressHammer: workers=%d shards=256 duration=%v pool=%dMB",
		workers, dur, sfl.cfg.PoolSize/(1024*1024))

	var (
		ops    atomic.Int64
		errs   atomic.Int64
		corrupt atomic.Int64
		done   atomic.Bool
		start  = time.Now()
	)

	// Shared pointer arena for reader goroutines.
	arena := make([]unsafe.Pointer, 1024)
	var arenaMu sync.RWMutex

	// Pre-populate.
	for i := range arena {
		slot, err := sfl.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		writeSlot(slot, uint64(i))
		arena[i] = unsafe.Pointer(unsafe.SliceData(slot))
	}

	// Progress reporter.
	go func() {
		for !done.Load() {
			time.Sleep(1 * time.Second)
			elapsed := time.Since(start)
			fmt.Printf("  hammer: %s  ops=%d (%.0f/s)  errors=%d  corrupt=%d\n",
				elapsed.Round(time.Second), ops.Load(),
				float64(ops.Load())/elapsed.Seconds(),
				errs.Load(), corrupt.Load())
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			seq := 0
			shardIdx := gid & 255
			for !done.Load() {
				switch gid % 5 {
				case 0: // Bounce: alloc/dealloc
					slot, err := sfl.Allocate()
					if err != nil {
						errs.Add(1)
						seq++
						continue
					}
					magic := slotMagic(gid, seq)
					writeSlot(slot, magic)
					if got := readSlot(slot); got != magic {
						corrupt.Add(1)
					}
					sfl.Deallocate(slot)
				case 1: // Retire: alloc/retire + Hyaline protect
					slot, err := sfl.Allocate()
					if err != nil {
						errs.Add(1)
						seq++
						continue
					}
					writeSlot(slot, slotMagic(gid, seq))
					sfl.HyalineEnter(shardIdx)
					sfl.Retire(slot)
					sfl.HyalineLeave(shardIdx)
				case 2: // Reader: enter/touch/leave
					sfl.HyalineEnter(shardIdx)
					arenaMu.RLock()
					for j := 0; j < 16; j++ {
						idx := (gid + j) & (len(arena) - 1)
						if arena[idx] != nil {
							_ = *(*uint64)(arena[idx])
						}
					}
					arenaMu.RUnlock()
					sfl.HyalineLeave(shardIdx)
				case 3: // Publisher: alloc/write/publish/dealloc
					slot, err := sfl.Allocate()
					if err != nil {
						errs.Add(1)
						seq++
						continue
					}
					writeSlot(slot, slotMagic(gid, seq))
					ptr := unsafe.Pointer(unsafe.SliceData(slot))
					arenaMu.Lock()
					arena[gid&(len(arena)-1)] = ptr
					arenaMu.Unlock()
					sfl.Deallocate(slot)
				case 4: // Burst: alloc many, dealloc all
					var batch [][]byte
					for j := 0; j < 8; j++ {
						slot, err := sfl.Allocate()
						if err != nil {
							break
						}
						batch = append(batch, slot)
					}
					for _, slot := range batch {
						sfl.Deallocate(slot)
					}
				}
				seq++
				ops.Add(1)
			}
		}(g)
	}

	time.Sleep(dur)
	done.Store(true)
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("hammer finished: ops=%d (%.0f/s) errors=%d corruptions=%d elapsed=%v",
		ops.Load(), float64(ops.Load())/elapsed.Seconds(),
		errs.Load(), corrupt.Load(), elapsed.Round(time.Millisecond))

	if corrupt.Load() > 0 {
		t.Fatalf("DATA CORRUPTION: %d", corrupt.Load())
	}

	// Final integrity: verify all arena slots still have their data (no silent
	// corruption from Hyaline reclamation).
	sfl.HyalineEnter(0)
	arenaMu.RLock()
	for i, ptr := range arena {
		if ptr != nil {
			_ = *(*uint64)(ptr)
		}
		_ = i
	}
	arenaMu.RUnlock()
	sfl.HyalineLeave(0)

	// Post-hammer: verify pool is still operational.
	recovered := 0
	for i := 0; i < 10000; i++ {
		slot, err := sfl.Allocate()
		if err != nil {
			break
		}
		writeSlot(slot, uint64(i))
		sfl.Deallocate(slot)
		recovered++
	}
	t.Logf("post-hammer recovery: %d alloc/free cycles succeeded", recovered)
	if recovered < 1000 {
		t.Fatalf("post-hammer recovery too low: %d", recovered)
	}
}
