#!/bin/bash
# Checkout-only tooling; never installed with the production executable.
set -euo pipefail
source "$(dirname "$0")/common.sh"
need go 'Install Go 1.27.1 and run make setup.'
exec go run -mod=readonly ./internal/devtools/release "$@"
