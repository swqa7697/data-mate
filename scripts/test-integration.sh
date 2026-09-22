#!/bin/bash
set -euo pipefail
if [[ -n "${CI:-}" ]]; then
  echo 'Integration tests are excluded from CI.' >&2
  exit 1
fi
echo 'NOT_READY: Docker PostgreSQL integration begins in P3; no integration tests ran.' >&2
exit 1
