//go:build linux && arm64
package memory

// CacheLineSize is 64 bytes on standard ARM64 processors like AWS Graviton.
const CacheLineSize = 64
