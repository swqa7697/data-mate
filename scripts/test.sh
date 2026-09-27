#!/bin/bash
set -euo pipefail
# Owned-path fixtures and executable peers must not inherit a group-writable umask.
umask 077
source "$(dirname "$0")/common.sh"
# Runtime fixtures use t.TempDir; tests never require user credentials or a DB.
# With dependencies installed, the normal suite must not reach the network.
export GOPROXY=off
export GOSUMDB=off
go test -mod=readonly "$@" ./...
