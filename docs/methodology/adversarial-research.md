# Adversarial Research Methodology

## Source

Primary paper:
[DriveFuzz: Discovering Autonomous Driving Bugs through Driving Quality-Guided Fuzzing](https://seulbae-security.github.io/pubs/drivefuzz-ccs22.pdf)

DriveFuzz is about autonomous driving systems, but the useful idea transfers to
this memory allocator at the methodology level: test the integrated system as a
black box, mutate realistic operating scenarios, define system-level oracles, and
guide search toward near-failure states with a domain-specific quality score.

## Transfer Model

DriveFuzz does not try to prove each autonomous driving layer correct in
isolation. It mutates full driving scenarios and watches the final physical
vehicle behavior: collisions, traffic violations, immobility, and reckless
vehicle dynamics.

For this allocator, the equivalent is to avoid testing only isolated functions or
single hand-written stress patterns. We should test the allocator as a full
stateful system:

- `ShardedFreeList`
- shard-local fresh and recycled caches
- global `FreeList`
- Hyaline retire and reclamation pipeline
- adaptive PID threshold controller
- reset/free lifecycle
- scheduler interaction through goroutines, `GOMAXPROCS`, and reader stalls

The allocator analogue of "vehicle state" is not one internal counter. It is the
observable runtime behavior of the whole allocator under pressure.

## System Oracles

DriveFuzz uses traffic rules as its test oracle. For this allocator, the oracles
should be explicit safety and liveness properties:

- no data corruption after allocate, write, read, deallocate, and reuse
- no duplicate live ownership of the same slot
- no undetected double free
- no invalid deallocation accepted as valid
- `Allocated` never exceeds `PoolSize`
- no permanent allocation failure after retired slots become reclaimable
- bounded recovery after pool exhaustion
- no leaked controller goroutines after `Free` or `Reset`
- no use-after-`Free` success path
- no threshold oscillation that causes sustained allocation failure
- no unreclaimed Hyaline backlog after readers have quiesced

These are more valuable than only checking throughput because they describe
allocator-level misbehavior regardless of which layer caused it.

## Allocator Quality Score

DriveFuzz guides mutation with a driving quality score. The allocator equivalent
should score "allocator health" for scenarios that do not yet violate an oracle.
Lower scores should mean the scenario is closer to failure and worth mutating.

Candidate quality inputs:

- minimum free-depth ratio during the run
- allocation error rate
- p95 and p99 allocation latency
- CAS retry rate
- maximum Hyaline retire batch depth
- total pending retired slots
- retire-to-reclaim lag
- forced reclamation count
- post-pressure recovery time
- PID threshold swing rate
- shard cache imbalance
- global free-list starvation while shard-local caches hold idle slots

The current PID controller mainly observes global `Reserved`, `Allocated`, and
`SlotSize`. That is intentionally simple, but it cannot directly see per-shard
Hyaline batches, shard imbalance, or retire-to-reclaim lag. A quality score gives
us a better way to discover whether those invisible states matter in practice.

## Scenario Mutation

DriveFuzz mutates maps, actors, weather, and road friction. Our fuzzer should
mutate realistic allocator workload parameters:

- pool size
- slot size
- slab size
- shard count
- `GOMAXPROCS`
- goroutine count
- allocate/deallocate/retire/read ratios
- reader hold time between `HyalineEnter` and `HyalineLeave`
- burst size
- live pointer table size
- preallocation on or off
- scheduler yield frequency
- reset/free timing in lifecycle tests

The goal is not random chaos. Mutations should preserve valid allocator usage
unless a scenario is intentionally testing invalid API calls. This mirrors
DriveFuzz's constraint that generated driving scenarios remain physically
plausible.

## Controller Implications

The DriveFuzz lesson for the PID controller is that a controller should be
evaluated by whole-system behavior, not only by the signal it directly controls.

The current controller lowers Hyaline flush threshold as global free depth
shrinks. That has already addressed the exhaustion cliff, but future controller
changes should be driven by adversarial scenarios that measure:

- whether threshold changes arrive before pool exhaustion
- whether forced reclamation becomes rare under sustained retire pressure
- whether the threshold stabilizes after pressure drops
- whether different allocators registered with the shared controller remain
independent
- whether low global free depth is caused by true live allocation or stranded
retired memory

Before making the controller more complex, add observability around the state it
is indirectly trying to control:

- current Hyaline threshold
- per-shard `fresh` depth
- per-shard `recycled` depth
- per-shard Hyaline `batch.counter`
- Hyaline flush count
- Hyaline leave-drain count
- force reclamation count
- allocation retry and exhaustion count

## Proposed Harness

A DriveFuzz-inspired allocator harness would run as a stress/property test:

1. Start from a known-valid workload seed.
2. Mutate one workload dimension.
3. Run the scenario for a short bounded duration.
4. Check hard oracles.
5. If no oracle fails, compute allocator quality.
6. Keep the worst valid scenario as the next seed.
7. Save any oracle-violating scenario as a reproducible seed.

Useful outputs:

- seed configuration
- random seed
- operation mix
- final allocator stats
- threshold timeline
- allocation error timeline
- force reclamation events
- shortest reproduction duration

This should complement, not replace, the existing hand-written stress tests.
Manual tests encode known hazards. Quality-guided adversarial tests search for
unknown bad interactions between the allocator, Hyaline, the PID controller, and
the Go scheduler.

