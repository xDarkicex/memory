//go:build !procpin

package memory

import _ "unsafe"

//go:linkname fastrand runtime.fastrand
func fastrand() uint32

// getShard returns a random shard index derived from runtime.fastrand().
// This approach distributes lock-free allocations rapidly across all available
// shards without requiring process-pinning (procPin). It mirrors the highly
// scalable per-CPU cache selection strategy used in Slabby.
//
// numShards must be a power of 2.
func getShard(numShards int) int {
	return int(fastrand()) & (numShards - 1)
}
