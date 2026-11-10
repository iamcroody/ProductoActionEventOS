#!/usr/bin/env bash
# Run the full Action EventOS test suite with the race detector. The race
# detector is essential here: the core guarantee is that concurrent operators
# can never double-discount a balance or drive inventory negative.
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/opt/homebrew/bin:$PATH"

echo "==> go vet"
go vet ./...

echo "==> go test -race ./..."
go test -race -count=1 ./...

echo "==> All tests passed."
