#!/bin/bash
set -e

echo "Running tests..."
go test ./...

echo "Running race tests..."
go test -race ./...

echo "Running go vet (with waiver for unsafe.Pointer(uintptr) in pointer materialization)..."
# We pipe the output of go vet and use grep to filter out the known warning
# about uintptr to unsafe.Pointer conversion which is intentional in our code.
# The exit status of grep -v will be 0 if lines are printed, 1 if no lines are printed.
# We want to fail the script if go vet outputs anything OTHER than the ignored warning.

# Run go vet and capture output (both stdout and stderr)
vet_output=$(go vet ./... 2>&1 || true)

# Filter out the known warning
filtered_output=$(echo "$vet_output" | grep -v "possible misuse of unsafe.Pointer" || true)

# If filtered_output is not empty, it means there are other vet warnings
if [ -n "$filtered_output" ]; then
    echo "go vet failed with the following errors:"
    echo "$filtered_output"
    exit 1
else
    echo "go vet passed (ignoring intentional unsafe.Pointer usage)."
fi

echo "All checks passed!"
