package memory

// SplitMix64 is an extremely fast, inlineable integer hash with a 10-14 cycle critical path.
// It matches the nanosecond latency constraints established for the OffHeapMap.
//
//go:nosplit
func SplitMix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
