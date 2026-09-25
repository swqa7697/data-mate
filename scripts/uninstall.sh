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
# The checkout wrapper owns the development container, not the data-root helper.
# Never remove contents here: unrelated files and symlinks must survive cleanup.
trim_dev_directory() {
  local dir="$project_dir/.dev"
  if [[ "${PURGE:-0}" == 1 && -d "$dir" && ! -L "$dir" && -O "$dir" ]]; then
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
# Always compile outside the root: a default uninstall may already have removed
# the executable, and a purge tombstone deliberately prevents reinstalling it.
check_build_deps
umask 077
helper_dir="$(mktemp -d /tmp/data-mate-uninstall.XXXXXX)"
helper="$helper_dir/data-mate"
complete=1
trap 'if [[ "$complete" == 1 ]]; then rm -rf "$helper_dir"; else printf "Cleanup helper retained for retry: %s\n" "$helper" >&2; printf "Retry: %q __uninstall --root %q" "$helper" "$root" >&2; if [[ "${PURGE:-0}" == 1 ]]; then printf " --purge" >&2; fi; printf "\n" >&2; fi' EXIT
go build -mod=readonly -trimpath -o "$helper" ./cmd/data-mate
chmod 700 "$helper"
complete=0
args=(__uninstall --root "$root")
if [[ "${PURGE:-0}" == 1 ]]; then args+=(--purge); fi
"$helper" "${args[@]}"
complete=1
trim_dev_directory
printf 'Uninstalled %s (PURGE=%s)\n' "$root" "${PURGE:-0}"
