# Cybernetic Off-Heap HashMap Implementation Notes

This document synthesizes the formal mathematical proofs and architectural constraints required to sustain billions of operations per second using the Off-Heap Lock-Free HashMap.

## 1. AVX-512 and MESI Padding (128-Byte)
The `Bucket` struct is strictly padded to 128 bytes.
1. **Apple Silicon**: M-Series chips use 128-byte cache lines. A 128-byte bucket mathematically prevents two separate threads from mutating adjacent buckets and causing a cache-coherency invalidate storm (false sharing).
2. **x86-64 (AVX-512)**: 128 is a multiple of 64. Any 128-byte boundary is inherently 64-byte aligned, providing native hardware alignment for 512-bit ZMM vector loading via `VMOVAPS` without general protection faults.

## 2. PID Control Feedback Loop
The map abandons rigid static resizing thresholds (e.g. 75%). A cybernetic Proportional-Integral-Derivative (PID) controller observes insertion times and collision probe lengths. It dictates the incremental background resize rate via the characteristic polynomial roots, maintaining bounded-input bounded-output (BIBO) stability under extreme heavy-tailed (Pareto) workload bursts.

## 3. GC Blinding
By utilizing `syscall.Syscall6(SYS_MMAP)` on Linux/Darwin and `VirtualAlloc` on Windows, we bypass the Go runtime allocator. Operations exclusively cast between `uintptr` and `unsafe.Pointer`, entirely evading heap scans.

## 4. SWAR Filtering
The 56/8 bit-split uses the bottom 8 bits of a cryptographic hash as a fingerprint, packing 7 fingerprints plus a Hopscotch control byte into a single 64-bit `uint64`. This yields emulated `_mm_cmpeq_epi8` execution across 8 slots in 2 clock cycles.
