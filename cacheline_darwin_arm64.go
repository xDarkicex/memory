//go:build darwin && arm64
package memory

// CacheLineSize is 128 bytes on Apple Silicon M-series processors.
// This prevents false sharing between SWAR groups on adjacent cache lines.
const CacheLineSize = 128
