//go:build procpin

package memory

import (
	_ "unsafe"
)

//go:linkname procPin runtime.procPin
func procPin() int

// getShard returns the P-bound shard index via runtime.procPin.
// The calling goroutine is pinned to its P, guaranteeing stable affinity.
// Requires: go build -tags procpin -ldflags=-checklinkname=0
func getShard(numShards int) int {
	return procPin() & (numShards - 1)
}
