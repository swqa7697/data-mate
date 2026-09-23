#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
"$project_dir/scripts/setup.sh"
exec "$project_dir/scripts/build.sh"
