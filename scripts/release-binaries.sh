#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TAG="${1:-}"
if [ -z "$TAG" ]; then
	echo "usage: scripts/release-binaries.sh <tag>" >&2
	exit 1
fi

REPO_SLUG="${REPO_SLUG:-luqmaan/tmux-agent-emoji}"
DIST_DIR="${DIST_DIR:-$ROOT/dist}"
TARGET="${TARGET:-main}"
mkdir -p "$DIST_DIR"

build_binary() {
	local goarch="$1"
	local output="$2"
	CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -ldflags="-s -w" -o "$output" .
}

AMD64_BIN="$DIST_DIR/tmux-ai-status-linux-amd64"
ARM64_BIN="$DIST_DIR/tmux-ai-status-linux-arm64"
CHECKSUMS="$DIST_DIR/tmux-ai-status-checksums.txt"

rm -f "$AMD64_BIN" "$ARM64_BIN" "$CHECKSUMS"
build_binary amd64 "$AMD64_BIN"
build_binary arm64 "$ARM64_BIN"
(cd "$DIST_DIR" && sha256sum "$(basename "$AMD64_BIN")" "$(basename "$ARM64_BIN")") >"$CHECKSUMS"

if gh release view "$TAG" --repo "$REPO_SLUG" >/dev/null 2>&1; then
	gh release upload "$TAG" "$AMD64_BIN" "$ARM64_BIN" "$CHECKSUMS" --clobber --repo "$REPO_SLUG"
else
	gh release create "$TAG" "$AMD64_BIN" "$ARM64_BIN" "$CHECKSUMS" --repo "$REPO_SLUG" --title "$TAG" --target "$TARGET"
fi
