#!/bin/bash
# Shared checkout paths and dependency diagnostics; sourcing never creates state.
set -euo pipefail
project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
export GOTOOLCHAIN=go1.27.1
export CGO_ENABLED=1
export GOOS=darwin
export GOARCH=arm64
cd "$project_dir"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s\n' "Missing $1. $2" >&2
    exit 1
  fi
}

check_build_deps() {
  need go 'Install Go 1.27.1 from https://go.dev/dl/.'
  need xcrun 'Install the Command Line Tools with xcode-select --install.'
  xcrun --find clang >/dev/null
  xcrun --show-sdk-path >/dev/null
  go version
  printf 'Target: %s/%s; host: %s\n' "$GOOS" "$GOARCH" "$(uname -m)"
}

# Existing installation directories must be owned private directories, not links.
private_dir() {
  local dir="$1"
  if [[ -L "$dir" ]]; then
    printf 'Refusing symlink directory: %s\n' "$dir" >&2
    exit 1
  fi
  if [[ ! -e "$dir" ]]; then mkdir -m 700 "$dir"; fi
  if [[ ! -d "$dir" || ! -O "$dir" || "$(stat -f '%Lp' "$dir")" != 700 ]]; then
    printf 'Expected an owned mode-0700 directory: %s\n' "$dir" >&2
    exit 1
  fi
}
