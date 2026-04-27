# Contributing

Contributions are welcome. This is a low-level, performance-sensitive Go
library — keeping changes focused and well-tested helps maintain trust.

## Before you start

- For anything beyond a small fix, open an issue first to discuss the
  approach. Surprise PRs that change public API surface or safety contracts
  are likely to need rework.
- Read through the README to understand the memory model, safety contracts,
  and benchmark methodology before proposing changes.

## Preparing a PR

Run these locally before pushing:

```
go test ./...
go test -race ./...
go vet ./...
```

## What reviewers look for

- **Tests pass.** Both `go test` and `go test -race` must be clean.
- **Safety contracts are preserved.** The README documents caller
  responsibilities (Reset quiescence, use-after-free rules, single-producer
  Arena). If your change alters these, update the docs.
- **Benchmarks stay honest.** If you change allocation paths, re-run the
  benchmarks and update the README numbers. Do not adjust benchmark
  scaffolding to make numbers look better.
- **Example tests stay correct.** If you change the API surface, update
  `example_test.go` and the examples under `examples/`.
- **Keep PRs focused.** One concern per PR. Refactors mixed with features
  are hard to review.

## Style

Follow the conventions already in the codebase: no unnecessary comments,
short names for local variables, direct language. The repo avoids
abstraction-for-abstraction's-sake.
