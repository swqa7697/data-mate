#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
check_build_deps
go mod download
# Compile the exact go.mod tool pins; no system packages or global binaries.
go tool shfmt --version
go tool staticcheck -version
exec "$project_dir/scripts/build.sh"
