#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
check_build_deps
download_args=(mod download)
if [[ "${VERBOSE:-0}" == 1 ]]; then
  download_args+=(-x)
fi
go "${download_args[@]}"
# Compile the exact go.mod tool pins; no system packages or global binaries.
go tool shfmt --version
go tool staticcheck -version
