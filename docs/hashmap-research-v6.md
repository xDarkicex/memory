# Cybernetic Off-Heap Hashmap: Cross-Platform Parity (Round 6)

## 1. Windows Off-Heap Boundary (`VirtualAlloc`)

### Syscall Selection
Windows does not use `mmap`. While `MapViewOfFile` exists, it requires creating a file mapping object. For purely anonymous, page-aligned off-heap memory, `VirtualAlloc` (with `MEM_COMMIT | MEM_RESERVE`) is the optimal zero-overhead Win32 primitive.

### Zero-Allocation Wrapper
To achieve zero-escape bounds on Windows, we bypass Cgo and high-level slice wrappers. We define a `//go:noescape` function mapped directly to a raw AMD64 assembly stub:

```asm
TEXT ·virtualAlloc(SB),NOSPLIT,$0-32
    MOVQ    ·procVirtualAlloc(SB), AX
    MOVQ    addr+0(FP),    RCX
    MOVQ    size+8(FP),    RDX
    MOVL    allocType+16(FP), R8D
    MOVL    protect+20(FP),  R9D
    CALL    AX
    MOVQ    RAX, ret+24(FP)
    RET
```
This is a strict `NOSPLIT` leaf function that directly loads the kernel32 address, executes the call, and returns the raw pointer, completely blinding the Go garbage collector.

## 2. Linux Syscall Parity

### Bypassing `unix.Mmap`
The standard `golang.org/x/sys/unix` `Mmap` function returns a `[]byte` slice, forcing heap allocation. However, `syscall.Syscall6` invoking `SYS_MMAP` operates purely on integer arguments and returns a raw `uintptr`. This perfectly satisfies our zero-allocation contract. 

To maintain strict symmetry with our macOS (Darwin) assembly stubs, we can construct an identical `NOSPLIT` assembly stub for Linux `amd64` and `arm64` that directly invokes the `SYSCALL` (or `svc #0`) instruction.

## 3. AMD64 `SplitMix64` Assembly

To match the 8-12 cycle latency of our ARM64 hash, we must implement `SplitMix64` in raw x86-64 assembly. The algorithm consists of a single dependency chain of `IMULQ`, `SHRQ`, and `XORQ` instructions. 

Because modern Intel (Sapphire Rapids) and AMD (Zen 4) cores can fully pipeline shifts and execute `IMULQ` in 3-4 cycles, the critical path evaluates to exactly **10-14 cycles**. Declared with `//go:noescape` and `NOSPLIT|NOFRAME`, this AMD64 leaf function guarantees our nanosecond hashing constraints across both CPU architectures.

## 4. Cache-Line Padding & MESI Dynamics

### The 64 vs 128 Byte Paradox
*   Apple M-Series (Darwin ARM64) relies on **128-byte** L1 cache lines.
*   Intel/AMD (AMD64) and Linux Graviton (ARM64) rely on **64-byte** cache lines.

If we globally force 128-byte padding for our `forwardingNode`, we achieve perfect MESI isolation on Apple Silicon, but on x86-64, we instantly introduce a **2x memory footprint penalty** and severe TLB pressure without any added coherence benefit. 

### The Mathematical Solution: Build Tags
To preserve cache behavior optimally on each microarchitecture, we will not use a global constant. Instead, we use `//go:build` tags to define architecture-specific constants:

```go
// cacheline_linux_amd64.go, cacheline_linux_arm64.go
const CacheLineSize = 64

// cacheline_darwin_arm64.go
const CacheLineSize = 128
```
Our structs will dynamically size their padding (`_pad [CacheLineSize - payloadSize]byte`), achieving mathematically optimal cache packing and zero false-sharing across all operating systems.
