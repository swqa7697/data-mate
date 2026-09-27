#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
case "${PURGE:-0}" in
0 | 1) ;;
*)
  echo 'PURGE must be 0 or 1.' >&2
  exit 2
  ;;
esac
# The checkout wrapper owns the development container, not the application data.
# Never remove contents here: unrelated files and symlinks must survive cleanup.
trim_dev_directory() {
  local dir="$project_dir/.dev"
  if [[ "${PURGE:-0}" == 1 && -d "$dir" && ! -L "$dir" ]]; then
    rmdir "$dir" 2>/dev/null || true
  fi
}
root="$project_dir/.dev/data-mate"
if [[ ! -e "$root" && ! -L "$root" ]]; then
  if [[ -e "$project_dir/.dev/bin/data-mate" || -L "$project_dir/.dev/bin/data-mate" ]]; then
    echo 'Cleanup cannot verify executable ownership without its data directory; preserved executable.' >&2
    exit 1
  fi
  trim_dev_directory
  exit 0
fi
private_dir "$root"
# Use the installed executable. A default uninstall may have removed it, so
# make can build a disposable runner for a later purge; no helper is retained.
runner="$project_dir/.dev/bin/data-mate"
if [[ ! -x "$runner" ]]; then
  check_build_deps
  umask 077
  runner_dir="$(mktemp -d /tmp/data-mate-uninstall.XXXXXX)"
  trap 'rm -rf "$runner_dir"' EXIT
  runner="$runner_dir/data-mate"
  go build -mod=readonly -trimpath -o "$runner" ./cmd/data-mate
fi
args=(__uninstall --root "$root")
if [[ "${PURGE:-0}" == 1 ]]; then args+=(--purge); fi
"$runner" "${args[@]}"
trim_dev_directory
printf 'Uninstalled %s (PURGE=%s)\n' "$root" "${PURGE:-0}"
