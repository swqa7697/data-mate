#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
if ! go tool staticcheck -version >/dev/null 2>&1; then
  echo 'Pinned staticcheck is unavailable. Run make install (or go mod download).' >&2
  exit 1
fi
go vet ./...
go tool staticcheck ./...
