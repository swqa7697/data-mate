#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
check_build_deps
umask 077
version="$(cat VERSION)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  echo 'VERSION must contain a single semantic version.' >&2
  exit 1
fi
revision=unknown
dirty=unknown
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  revision="$(git rev-parse --short=12 HEAD)"
  dirty=false
  if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then dirty=true; fi
fi
private_dir "$project_dir/.dev"
private_dir "$project_dir/.dev/bin"
target="$project_dir/.dev/bin/data-mate"
if [[ -e "$target" || -L "$target" ]]; then
  if [[ -L "$target" || ! -f "$target" || ! -O "$target" ]]; then
    echo 'Installed output is not an owned regular file.' >&2
    exit 1
  fi
fi
temporary="$(mktemp "$project_dir/.dev/bin/.data-mate.XXXXXX")"
trap 'rm -f "$temporary"' EXIT
flags="-X main.version=$version -X main.revision=$revision -X main.dirty=$dirty"
build_args=(build -mod=readonly -trimpath)
if [[ "${VERBOSE:-0}" == 1 ]]; then build_args+=(-x); fi
go "${build_args[@]}" -ldflags "$flags" -o "$temporary" ./cmd/data-mate
chmod 700 "$temporary"
mv -f "$temporary" "$target"
printf 'Installed %s\n' "$target"
