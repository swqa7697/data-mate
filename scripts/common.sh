#!/bin/bash
# Shared checkout paths and dependency diagnostics; sourcing never creates state.
set -euo pipefail
project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
export GOTOOLCHAIN=go1.27.1
export CGO_ENABLED=1
case "$(uname -s)/$(uname -m)" in
Darwin/arm64) export GOOS=darwin GOARCH=arm64 ;;
Linux/x86_64) export GOOS=linux GOARCH=amd64 ;;
*)
  echo 'Supported build hosts: macOS arm64 and Linux x86_64.' >&2
  exit 1
  ;;
esac
cd "$project_dir"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s\n' "Missing $1. $2" >&2
    exit 1
  fi
}

check_build_deps() {
  need go 'Install Go 1.27.1 from https://go.dev/dl/.'
  if [[ "$GOOS" == darwin ]]; then
    need xcrun 'Install the Command Line Tools with xcode-select --install.'
    xcrun --find clang >/dev/null
    xcrun --show-sdk-path >/dev/null
  else
    need "${CC:-cc}" 'Install a C compiler and libc development headers (Ubuntu: build-essential).'
  fi
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
  if [[ ! -d "$dir" || ! -O "$dir" || "$(file_mode "$dir")" != 700 ]]; then
    printf 'Expected an owned mode-0700 directory: %s\n' "$dir" >&2
    exit 1
  fi
}

file_mode() {
  if [[ "$GOOS" == darwin ]]; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}
