#!/usr/bin/env bash
# Build Action EventOS. Produces a single self-contained executable per target
# with the web UI embedded. Pure-Go SQLite means no CGO and trivial
# cross-compilation to Windows.
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/opt/homebrew/bin:$PATH"

OUT="dist"
mkdir -p "$OUT"

# Build the frontend if present, so the embedded ./web/dist is current.
if [ -f web/package.json ]; then
  echo "==> Building frontend (Vite)…"
  ( cd web && npm install --silent && npm run build )
fi

VERSION="${1:-0.1.0}"
LDFLAGS="-s -w -X 'main.version=${VERSION}'"

echo "==> Building Windows (amd64) -> ${OUT}/ActionEventOS.exe"
GOOS=windows GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
  -o "${OUT}/ActionEventOS.exe" ./cmd/eventpos

echo "==> Building macOS (arm64) -> ${OUT}/ActionEventOS-macos"
GOOS=darwin GOARCH=arm64 go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
  -o "${OUT}/ActionEventOS-macos" ./cmd/eventpos

echo "==> Building Linux (amd64) -> ${OUT}/ActionEventOS-linux"
GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
  -o "${OUT}/ActionEventOS-linux" ./cmd/eventpos

echo "==> Done. Artifacts in ${OUT}/"
ls -lh "${OUT}/"
