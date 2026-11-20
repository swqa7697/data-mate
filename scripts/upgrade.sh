#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
# Limit discovery and updates to this checkout even inside a Go workspace.
export GOWORK=off
args=(--force)
case "${MAJOR:-0}" in
1 | true | yes) ;;
*) args+=(--no-major) ;;
esac
go tool go-mod-upgrade "${args[@]}"
go mod tidy
echo 'Review dependency and import changes, then run make lint and make test.'
