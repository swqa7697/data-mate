#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
# .build is the sole reserved rebuildable directory at P0. Never traverse .dev.
if [[ -L .build || (-e .build && (! -d .build || ! -O .build)) ]]; then
  echo 'Expected an owned .build directory, not a link or other file.' >&2
  exit 1
fi
if [[ -d .build ]]; then rm -rf -- "$project_dir/.build"; fi
