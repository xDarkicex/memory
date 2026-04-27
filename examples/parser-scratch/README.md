# Parser Scratch Buffer

**Use case:** High-throughput JSON/DSL parsing where thousands of temporary tokens, string buffers, and AST nodes are allocated per parse and discarded together.

## The Problem

Standard Go parsing allocates per-parse scratch buffers on the heap:

```go
// scratch buffer must escape to heap because tokens reference it:
func tokenize(input string) ([]token, []byte) {
    buf := make([]byte, 0, 4096) // heap alloc (returned from func)
    // ... populate buf with string data ...
    return tokens, buf
}
```

At 10K requests/second, this generates ~10K heap allocations/second. The GC scans all of them, even though they're dead after the parse completes. The tokens and buffer must stay alive together — bulk lifetime, individual GC cost.

## The Arena Solution

One bulk allocation from mmap'd memory. One bulk free with `Reset()`. Zero GC interaction.

```go
pool, _ := memory.NewPool(memory.AllocatorConfig{...})
data, _ := pool.Allocate(4096) // off-heap scratch buffer
tokens := make([]token, 0, 32) // stack-allocated slice header

// ...parse N requests...

pool.Reset() // bulk-free all scratch memory
```

## Benchmark

| Approach | ns/op | B/op | allocs/op |
|---|---|---|---|
| Arena parser | 2213 | 0 | 0 |
| Standard (heap) | 397 | 4096 | 1 |

The arena version does **zero heap allocations per parse** — the scratch buffer lives in mmap'd memory invisible to the GC. The standard version heap-allocates a 4KB buffer that escapes to heap (returned from the function, since callers need the string data to outlive the call).

**Tradeoff:** Individual arena allocations are slower than heap allocations (mmap vs bump-pointer), so ns/op is higher for single-shot use. The win is at scale — when you have tens of thousands of parses per second across many goroutines, the GC scanning cost of heap-allocated scratch buffers dominates. Arena gives you predictable latency with zero GC cycles.
