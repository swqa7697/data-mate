#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
need go 'Install Go 1.27.1 from https://go.dev/dl/.'
if ! go tool shfmt --version >/dev/null 2>&1; then
  echo 'Pinned shfmt is unavailable. Run make install (or go mod download).' >&2
  exit 1
fi
go_files=()
while IFS= read -r -d '' file; do go_files+=("$file"); done < <(find cmd internal -type f -name '*.go' -print0)
if [[ "${1:-}" == --check ]]; then
  unformatted="$(gofmt -l "${go_files[@]}")"
  if [[ -n "$unformatted" ]]; then
    printf 'Run make format:\n%s\n' "$unformatted" >&2
    exit 1
  fi
  go tool shfmt -d -i 2 scripts
else
  gofmt -w "${go_files[@]}"
  go tool shfmt -w -i 2 scripts
fi
