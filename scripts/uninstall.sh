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
root="$project_dir/.dev"
if [[ ! -e "$root" && ! -L "$root" ]]; then exit 0; fi
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
printf 'Uninstalled %s (PURGE=%s)\n' "$root" "${PURGE:-0}"
